// Package config loads pg-proxy's ConfigMap-mounted YAML configuration and
// hot-reloads it (design §8). Reload is periodic-reread-plus-hash-compare,
// not fsnotify — see Store's doc comment for why.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"sigs.k8s.io/yaml"
)

// Duration wraps time.Duration so it unmarshals from YAML strings like
// "30s" (encoding/json — which sigs.k8s.io/yaml delegates to — has no
// built-in support for that).
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("config: duration must be a string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// Config is the full ConfigMap schema (design §8.1). listen, metricsListen,
// and the TLS file paths require a restart to take effect; everything under
// Limits, Timeouts, and Shutdown is hot-reloadable.
type Config struct {
	Listen        string              `json:"listen"`
	MetricsListen string              `json:"metricsListen"`
	TLS           TLSConfig           `json:"tls"`
	Backend       BackendConfig       `json:"backend"`
	Route         RouteConfig         `json:"route"`
	Limits        LimitsConfig        `json:"limits"`
	Timeouts      TimeoutsConfig      `json:"timeouts"`
	Shutdown      ShutdownConfig      `json:"shutdown"`
	ProxyProtocol ProxyProtocolConfig `json:"proxyProtocol"`
}

type TLSConfig struct {
	CertFile       string   `json:"certFile"`
	KeyFile        string   `json:"keyFile"`
	ReloadInterval Duration `json:"reloadInterval"`
}

// BackendConfig has no "mode" field: design §6.4 mandates standard TLS
// negotiation (SSLRequest, then a normal handshake) with no alternative —
// direct TLS is explicitly out of scope since it would additionally
// require PgBouncer >= 1.25. A config key with only one possible value and
// no code path that reads it would just be a silent lie if it existed.
type BackendConfig struct {
	CAFile string `json:"caFile"`
}

type RouteConfig struct {
	Namespace          string `json:"namespace"`
	HostnameAnnotation string `json:"hostnameAnnotation"`
	ClusterAnnotation  string `json:"clusterAnnotation"`
}

// LimitsConfig holds the two per-pod connection caps plus the CancelRequest
// abuse-protection bounds. All hot-reloadable.
type LimitsConfig struct {
	MaxConns           int          `json:"maxConns"`
	MaxConnsPerCluster int          `json:"maxConnsPerCluster"`
	Cancel             CancelConfig `json:"cancel"`
}

// CancelConfig bounds the CancelRequest path, which design §6.1 deliberately
// exempts from Authorizer/Registry/connection limits but which still costs
// a full backend TLS dial per request with no other ceiling. MaxPerIP <= 0
// disables the per-source-IP check — meaningful only once PROXY protocol
// recovers real client addresses; by default every connection shares
// APISIX's pod IP, making a per-IP limit a de facto global one.
type CancelConfig struct {
	Window      Duration `json:"window"`
	MaxPerIP    int      `json:"maxPerIP"`
	MaxInFlight int      `json:"maxInFlight"`
}

// TimeoutsConfig holds the three connection timeouts. Hot-reloadable.
type TimeoutsConfig struct {
	Handshake   Duration `json:"handshake"`
	BackendDial Duration `json:"backendDial"`
	Idle        Duration `json:"idle"`
}

// ShutdownConfig holds the graceful-shutdown drain timeout. Hot-reloadable.
type ShutdownConfig struct {
	DrainTimeout Duration `json:"drainTimeout"`
}

// ProxyProtocolConfig controls PROXY protocol v1/v2 parsing (design §9.4c).
// TrustedCIDRs is the allowlist of L4 proxy source addresses whose PROXY
// header pg-proxy will honor — accepting one from an arbitrary peer would
// let any direct client spoof client_ip in the audit log, exactly the
// field phase 2's authorization reconciliation consumes. Enabling the
// feature with an empty TrustedCIDRs trusts no one and rejects every
// connection (fail closed), rather than silently trusting everyone.
type ProxyProtocolConfig struct {
	Enabled      bool     `json:"enabled"`
	TrustedCIDRs []string `json:"trustedCIDRs"`
}

// Design §8.1's documented defaults. A ConfigMap that omits a field gets
// these, not the zero value — every one of maxConns/maxConnsPerCluster/
// handshake/backendDial/idle/drainTimeout is load-bearing at zero (see
// validate's doc comment), so "omitted" must not be indistinguishable from
// "explicitly disabled".
const (
	defaultListen             = ":5432"
	defaultMetricsListen      = ":9090"
	defaultMaxConns           = 5000
	defaultMaxConnsPerCluster = 500
	defaultHandshakeTimeout   = 10 * time.Second
	defaultBackendDialTimeout = 5 * time.Second
	defaultIdleTimeout        = 300 * time.Second
	defaultDrainTimeout       = 300 * time.Second
	defaultTLSReloadInterval  = 30 * time.Second

	// Cancel defaults. MaxPerIP is only actually applied when
	// proxyProtocol.enabled recovers real client addresses (see
	// cmd/pg-proxy's cancelLimitsFrom) — by default every connection shares
	// APISIX's pod IP, so a per-IP cap would otherwise be a silent
	// fleet-wide throttle on query cancellation.
	defaultCancelWindow      = 10 * time.Second
	defaultCancelMaxPerIP    = 5
	defaultCancelMaxInFlight = 50
)

// parse unmarshals raw YAML bytes into a Config, applies design §8.1's
// defaults to any field the YAML left at its zero value, and validates the
// result.
func parse(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaults fills in design §8.1's documented defaults for any field
// still at its zero value. It runs before validate, so an explicit
// negative value is still rejected rather than silently defaulted.
func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.MetricsListen == "" {
		c.MetricsListen = defaultMetricsListen
	}
	if c.Limits.MaxConns == 0 {
		c.Limits.MaxConns = defaultMaxConns
	}
	if c.Limits.MaxConnsPerCluster == 0 {
		c.Limits.MaxConnsPerCluster = defaultMaxConnsPerCluster
	}
	if c.Timeouts.Handshake == 0 {
		c.Timeouts.Handshake = Duration(defaultHandshakeTimeout)
	}
	if c.Timeouts.BackendDial == 0 {
		c.Timeouts.BackendDial = Duration(defaultBackendDialTimeout)
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = Duration(defaultIdleTimeout)
	}
	if c.Shutdown.DrainTimeout == 0 {
		c.Shutdown.DrainTimeout = Duration(defaultDrainTimeout)
	}
	if c.TLS.ReloadInterval == 0 {
		c.TLS.ReloadInterval = Duration(defaultTLSReloadInterval)
	}
	if c.Limits.Cancel.Window == 0 {
		c.Limits.Cancel.Window = Duration(defaultCancelWindow)
	}
	if c.Limits.Cancel.MaxPerIP == 0 {
		c.Limits.Cancel.MaxPerIP = defaultCancelMaxPerIP
	}
	if c.Limits.Cancel.MaxInFlight == 0 {
		c.Limits.Cancel.MaxInFlight = defaultCancelMaxInFlight
	}
}

// validate rejects structurally-parseable but semantically invalid config,
// most importantly negative timeouts.
func (c *Config) validate() error {
	durations := map[string]Duration{
		"tls.reloadInterval":    c.TLS.ReloadInterval,
		"timeouts.handshake":    c.Timeouts.Handshake,
		"timeouts.backendDial":  c.Timeouts.BackendDial,
		"timeouts.idle":         c.Timeouts.Idle,
		"shutdown.drainTimeout": c.Shutdown.DrainTimeout,
		"limits.cancel.window":  c.Limits.Cancel.Window,
	}
	for field, d := range durations {
		if d.Duration() < 0 {
			return fmt.Errorf("config: %s must not be negative, got %s", field, d.Duration())
		}
	}
	if c.Limits.MaxConns < 0 {
		return fmt.Errorf("config: limits.maxConns must not be negative, got %d", c.Limits.MaxConns)
	}
	if c.Limits.MaxConnsPerCluster < 0 {
		return fmt.Errorf("config: limits.maxConnsPerCluster must not be negative, got %d", c.Limits.MaxConnsPerCluster)
	}
	// 0 is CancelRateLimiter's documented "disabled" sentinel for both
	// fields (see CancelConfig's doc comment) — a negative number isn't a
	// different, more-disabled state, it's malformed input that would
	// silently behave the same as 0 in server.CancelRateLimiter.Allow's
	// "<= 0" checks, most dangerously for maxInFlight: a stray "-1" from a
	// bad hot-reload edit would silently turn off the one
	// topology-independent ceiling CancelRequest abuse protection has left.
	if c.Limits.Cancel.MaxPerIP < 0 {
		return fmt.Errorf("config: limits.cancel.maxPerIP must not be negative, got %d", c.Limits.Cancel.MaxPerIP)
	}
	if c.Limits.Cancel.MaxInFlight < 0 {
		return fmt.Errorf("config: limits.cancel.maxInFlight must not be negative, got %d", c.Limits.Cancel.MaxInFlight)
	}
	for _, cidr := range c.ProxyProtocol.TrustedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: proxyProtocol.trustedCIDRs entry %q is not a valid CIDR: %w", cidr, err)
		}
	}
	// isTrustedProxySource trusts nothing when TrustedProxyNets is empty, so
	// enabling the feature without an allowlist fails closed at runtime —
	// every single connection gets rejected. Catch that at load time rather
	// than letting it surface only once traffic hits the pod.
	if c.ProxyProtocol.Enabled && len(c.ProxyProtocol.TrustedCIDRs) == 0 {
		return fmt.Errorf("config: proxyProtocol.enabled is true but trustedCIDRs is empty — this would reject every connection")
	}
	return nil
}

// Load reads and parses the config file once, for use at startup. A
// failure here is expected to be fatal — there is no "old value" to fall
// back to yet.
func Load(path string) (*Config, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parse(raw)
}
