// Command pg-proxy is the entry point for the pg-proxy service: it loads
// configuration, wires the four seams (Router, Authorizer, Registry, Relay)
// together, and runs the connection-accept loop until shutdown.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/config"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/route"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/server"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/sysutil"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/tlsutil"
)

// configPathEnv names the environment variable pointing at the mounted
// ConfigMap's YAML file; defaultConfigPath is used when it's unset.
const (
	configPathEnv     = "PGPROXY_CONFIG"
	defaultConfigPath = "/etc/pgproxy/config.yaml"
)

// routeResyncPeriod is the informer's periodic resync — a heartbeat that
// keeps pgproxy_route_table_last_sync_seconds fresh even when nothing
// actually changes (design §9.5).
const routeResyncPeriod = 30 * time.Second

// metricsPollInterval is how often gauges reflecting external state
// (cert expiry, route table staleness) are refreshed.
const metricsPollInterval = 10 * time.Second

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	if err := run(logger); err != nil {
		logger.Println("pg-proxy:", err)
		os.Exit(1)
	}
}

// deps holds every long-lived component run assembles and then serves.
type deps struct {
	cfgStore   *config.Store
	certStore  *tlsutil.Store
	router     *route.InformerRouter
	metrics    *metrics.Metrics
	prober     *metrics.Prober
	listener   *server.Listener
	metricsSrv *http.Server
}

// run is split out from main so tests could exercise it without os.Exit.
func run(logger *log.Logger) error {
	nofile, err := sysutil.RaiseNofileLimit(logger)
	if err != nil {
		return fmt.Errorf("raise nofile limit: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	d, err := buildDeps(ctx, nofile)
	if err != nil {
		return err
	}

	cfg := d.cfgStore.Current()
	logger.Printf("pg-proxy: listening on %s (metrics on %s)", cfg.Listen, cfg.MetricsListen)
	return serveUntilShutdown(ctx, logger, d)
}

// buildDeps loads configuration and certificates, connects to the k8s API,
// and wires the four seams together. It returns once the route informer's
// initial cache sync completes (design §7.4) — a failure anywhere here is
// meant to crash the process rather than serve from partial state.
func buildDeps(ctx context.Context, nofile sysutil.NofileResult) (*deps, error) {
	configPath := os.Getenv(configPathEnv)
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfgStore, err := config.NewStore(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", configPath, err)
	}
	cfg := cfgStore.Current()

	certStore, err := tlsutil.NewStoreWithInterval(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.ReloadInterval.Duration())
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	backendCA, err := loadCAPool(cfg.Backend.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load backend CA: %w", err)
	}

	router, err := startRouter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	limits := limitsFrom(cfgStore)
	met := metrics.New()
	applyStartupGauges(met, nofile)
	prober := metrics.NewProber(router.Ready)
	cancelLimits := cancelLimitsFrom(cfgStore)
	handler := buildHandler(cfg, router, certStore, backendCA, met, limits, cancelLimits)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	return &deps{
		cfgStore:   cfgStore,
		certStore:  certStore,
		router:     router,
		metrics:    met,
		prober:     prober,
		listener:   server.NewListener(ln, handler),
		metricsSrv: metrics.NewServer(cfg.MetricsListen, met, prober),
	}, nil
}

// startRouter builds the k8s clients and the Pooler informer, then blocks
// until its initial cache sync completes (design §7.4).
func startRouter(ctx context.Context, cfg *config.Config) (*route.InformerRouter, error) {
	dynamicClient, k8sClient, err := newK8sClients()
	if err != nil {
		return nil, err
	}
	router := route.NewInformerRouter(dynamicClient, route.InformerConfig{
		Namespace:          cfg.Route.Namespace,
		HostnameAnnotation: cfg.Route.HostnameAnnotation,
		ClusterAnnotation:  cfg.Route.ClusterAnnotation,
		ResyncPeriod:       routeResyncPeriod,
	}, route.NewK8sEventRecorder(k8sClient))
	if err := router.Start(ctx); err != nil {
		return nil, fmt.Errorf("start route informer: %w", err)
	}
	return router, nil
}

// applyStartupGauges records one-time startup measurements as metrics.
// Split out from buildDeps so it's testable without a real k8s cluster,
// TLS certs, or a listening socket — connection-lifecycle's「啟動時提高
// nofile 上限」requires the raised limit to be exposed as
// pgproxy_nofile_limit, not just logged.
func applyStartupGauges(met *metrics.Metrics, nofile sysutil.NofileResult) {
	met.SetNofileLimit(float64(nofile.After))
}

// limitsFrom adapts the hot-reloadable config store into the
// registry.LimitsFunc shape both the Registry and the Handler's early
// admission check read from.
func limitsFrom(cfgStore *config.Store) registry.LimitsFunc {
	return func() registry.Limits {
		c := cfgStore.Current()
		return registry.Limits{MaxConns: c.Limits.MaxConns, MaxConnsPerCluster: c.Limits.MaxConnsPerCluster}
	}
}

// buildHandler assembles the four seams into a server.Handler.
func buildHandler(cfg *config.Config, router *route.InformerRouter, certStore *tlsutil.Store, backendCA *x509.CertPool, met *metrics.Metrics, limits registry.LimitsFunc, cancelLimits server.CancelLimitsFunc) *server.Handler {
	return &server.Handler{
		Router:     router,
		Authorizer: authz.NewAllowAll(),
		Registry:   registry.NewInMemoryRegistry(limits),
		Relay:      relay.New(cfg.Timeouts.Idle.Duration()),
		Audit:      audit.NewSink(os.Stdout),
		Metrics:    met,
		ClientTLS: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: certStore.GetCertificate,
		},
		BackendTLS: &tls.Config{RootCAs: backendCA},
		Timeouts: server.Timeouts{
			Handshake:   cfg.Timeouts.Handshake.Duration(),
			BackendDial: cfg.Timeouts.BackendDial.Duration(),
			Idle:        cfg.Timeouts.Idle.Duration(),
		},
		ProxyProtocolEnabled: cfg.ProxyProtocol.Enabled,
		TrustedProxyNets:     trustedProxyNets(cfg.ProxyProtocol.TrustedCIDRs),
		CancelLimiter:        server.NewCancelRateLimiter(cancelLimits),
	}
}

// cancelLimitsFrom adapts the hot-reloadable config store into the
// server.CancelLimitsFunc shape CancelRateLimiter reads from. MaxPerIP is
// forced to 0 (disabled) whenever PROXY protocol is off, regardless of the
// configured value: a per-source-IP cap is only meaningful once
// TrustedProxyNets recovers a real client address, and with PROXY protocol
// off every connection shares APISIX's one pod IP, making a per-IP limit a
// silent fleet-wide throttle on legitimate query cancellation instead of
// abuse protection (see config.CancelConfig's doc comment).
func cancelLimitsFrom(cfgStore *config.Store) server.CancelLimitsFunc {
	return func() server.CancelLimits {
		c := cfgStore.Current()
		maxPerIP := c.Limits.Cancel.MaxPerIP
		if !c.ProxyProtocol.Enabled {
			maxPerIP = 0
		}
		return server.CancelLimits{
			Window:      c.Limits.Cancel.Window.Duration(),
			MaxPerIP:    maxPerIP,
			MaxInFlight: c.Limits.Cancel.MaxInFlight,
		}
	}
}

// trustedProxyNets parses the config's validated CIDR strings. Entries are
// already confirmed parseable by Config.validate() at load time; a defensive
// re-check here just skips (rather than crashes on) any that somehow
// aren't, since a bad allowlist should fail closed, not panic.
func trustedProxyNets(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, ipnet)
		}
	}
	return nets
}

// newK8sClients builds the dynamic client (for Poolers — design §7.2
// deliberately avoids a typed CNPG client) and the typed clientset (for
// emitting core/v1 Events), both from the in-cluster service account.
func newK8sClients() (dynamic.Interface, kubernetes.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load in-cluster k8s config: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build dynamic k8s client: %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build k8s clientset: %w", err)
	}
	return dynamicClient, k8sClient, nil
}

// serveUntilShutdown starts every background loop, accepts connections
// until ctx is cancelled (SIGTERM/SIGINT), then drains (design §9.1).
//
// ctx is deliberately NEVER passed to d.listener.Serve() below — see
// Listener.Serve's doc comment for the round-2 critical this avoids
// reintroducing (signal cancellation tearing down live connections
// instantly instead of going through Shutdown's drain phase). ctx's only
// job here is deciding *when* the explicit d.listener.Shutdown(drainTimeout)
// call a few lines down runs; Serve's own signature makes it impossible to
// wire ctx into it by mistake.
func serveUntilShutdown(ctx context.Context, logger *log.Logger, d *deps) error {
	go func() {
		if err := d.metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("pg-proxy: metrics server stopped: %v", err)
		}
	}()
	go d.cfgStore.Run(ctx)
	go d.certStore.Run(ctx)
	go pollGauges(ctx, d.metrics, d.certStore, d.router)

	serveErr := make(chan error, 1)
	go func() { serveErr <- d.listener.Serve() }() // no ctx — see doc comment above

	// Serve only returns on its own for a permanent (non-temporary,
	// non-close) Accept error — the common case is ctx.Done() arriving
	// first via SIGTERM. If Serve dies unexpectedly, the accept loop is
	// gone even though the process is still running; flip readiness so k8s
	// stops routing new connections here instead of leaving the pod a
	// silent black hole (see design's readyz semantics — it otherwise only
	// consults informer sync and drain state, neither of which this
	// affects).
	var preShutdownErr error
	select {
	case <-ctx.Done():
		logger.Println("pg-proxy: received shutdown signal, draining")
	case preShutdownErr = <-serveErr:
		logger.Printf("pg-proxy: listener stopped unexpectedly, marking not ready: %v", preShutdownErr)
		d.prober.SetDraining(true)
		d.metrics.SetDraining(true)
		<-ctx.Done()
	}

	d.metrics.SetDraining(true)
	d.prober.SetDraining(true)
	d.listener.Shutdown(d.cfgStore.Current().Shutdown.DrainTimeout.Duration())
	logger.Println("pg-proxy: drain complete, exiting")

	if preShutdownErr == nil {
		if err := <-serveErr; err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Printf("pg-proxy: listener stopped with error: %v", err)
		}
	}
	return nil
}

// pollGauges refreshes gauges that reflect polled state (certificate
// expiry, route table size, informer sync staleness) rather than being
// updated as a side effect of some other call.
func pollGauges(ctx context.Context, met *metrics.Metrics, certStore *tlsutil.Store, router *route.InformerRouter) {
	ticker := time.NewTicker(metricsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			met.SetCertExpirySeconds(float64(certStore.NotAfter().Unix()))
			met.SetRouteTableEntries(float64(router.Len()))
			met.SetRouteTableLastSyncSeconds(router.LastSyncSeconds())
		}
	}
}

// loadCAPool reads a PEM-encoded CA bundle for verifying backend certs.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid certificates found in %s", path)
	}
	return pool, nil
}
