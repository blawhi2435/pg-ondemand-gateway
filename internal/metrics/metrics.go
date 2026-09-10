// Package metrics implements pg-proxy's Prometheus registry and the
// health/readiness endpoints served alongside it (design §9.5).
//
// Cardinality discipline: every metric here is labeled only by cluster (or
// a fixed-value-domain dimension like result/reason/direction). Nothing is
// labeled by user, database, client_ip, sni, or hostname — any of those
// would create one time series per account or per source address.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "pgproxy"

// Label value domains for connections_total's "result" label,
// handshake_errors_total's "reason" label, and bytes_total's "direction"
// label. Named constants, following internal/reason's pattern for
// disconnect reasons, so every caller shares one source of truth instead
// of independently-typed string literals that could silently drift into
// two different labels for what was meant to be the same outcome.
const (
	ResultOK            = "ok"
	ResultDenied        = "denied"
	ResultLimitExceeded = "limit_exceeded"
	ResultBackendError  = "backend_error"

	DirectionIn  = "in"
	DirectionOut = "out"

	ReasonBadStartup           = "bad_startup"
	ReasonTLSError             = "tls_error"
	ReasonUnknownSNI           = "unknown_sni"
	ReasonPlaintextRejected    = "plaintext_rejected"
	ReasonUntrustedProxySource = "untrusted_proxy_source"
	ReasonBadProxyHeader       = "bad_proxy_header"
	ReasonCancelRateLimited    = "cancel_rate_limited"
)

// Metrics owns pg-proxy's Prometheus registry and every metric in design
// §9.5. Each instance has its own registry (rather than the global default)
// so multiple instances — one per test — never collide on registration.
type Metrics struct {
	registry *prometheus.Registry

	connectionsActive      *prometheus.GaugeVec
	connectionsTotal       *prometheus.CounterVec
	handshakeErrorsTotal   *prometheus.CounterVec
	backendDialSeconds     *prometheus.HistogramVec
	bytesTotal             *prometheus.CounterVec
	certExpirySeconds      prometheus.Gauge
	draining               prometheus.Gauge
	nofileLimit            prometheus.Gauge
	routeTableEntries      prometheus.Gauge
	routeTableLastSyncSecs prometheus.Gauge
	auditErrorsTotal       prometheus.Counter
}

// newGauge/newGaugeVec/newCounterVec/newHistogramVec wrap the corresponding
// prometheus constructors, filling in the shared namespace so New's own
// metric list doesn't repeat it ten times over.
func newGauge(name, help string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
}

func newGaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

func newCounterVec(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

func newHistogramVec(name, help string, labels ...string) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help}, labels)
}

// New builds a Metrics with all design §9.5 metrics registered.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry:               reg,
		connectionsActive:      newGaugeVec("connections_active", "Currently active connections.", "cluster"),
		connectionsTotal:       newCounterVec("connections_total", "Total connections attempted, by outcome.", "cluster", "result"),
		handshakeErrorsTotal:   newCounterVec("handshake_errors_total", "First-packet/handshake failures, by reason.", "reason"),
		backendDialSeconds:     newHistogramVec("backend_dial_seconds", "Time spent dialing and TLS-handshaking the backend.", "cluster"),
		bytesTotal:             newCounterVec("bytes_total", "Bytes relayed, by direction.", "cluster", "direction"),
		certExpirySeconds:      newGauge("cert_expiry_seconds", "NotAfter (unix seconds) of the currently served TLS certificate."),
		draining:               newGauge("draining", "1 while pg-proxy is draining for shutdown, 0 otherwise."),
		nofileLimit:            newGauge("nofile_limit", "RLIMIT_NOFILE soft limit after startup adjustment."),
		routeTableEntries:      newGauge("route_table_entries", "Number of hostnames currently in the route table."),
		routeTableLastSyncSecs: newGauge("route_table_last_sync_seconds", "Seconds since the route table last synced successfully."),
		auditErrorsTotal:       prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "audit_errors_total", Help: "Audit events rejected by internal/audit's own validation (e.g. an out-of-domain disconnect reason) and therefore never written."}),
	}

	reg.MustRegister(
		m.connectionsActive, m.connectionsTotal, m.handshakeErrorsTotal, m.backendDialSeconds, m.bytesTotal,
		m.certExpirySeconds, m.draining, m.nofileLimit, m.routeTableEntries, m.routeTableLastSyncSecs,
		m.auditErrorsTotal,
	)
	return m
}

// Registry returns the underlying Prometheus registry, for mounting /metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

func (m *Metrics) IncActive(cluster string) {
	m.connectionsActive.WithLabelValues(cluster).Inc()
}

func (m *Metrics) DecActive(cluster string) {
	m.connectionsActive.WithLabelValues(cluster).Dec()
}

func (m *Metrics) IncConnectionsTotal(cluster, result string) {
	m.connectionsTotal.WithLabelValues(cluster, result).Inc()
}

func (m *Metrics) IncHandshakeError(reason string) {
	m.handshakeErrorsTotal.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveBackendDialSeconds(cluster string, seconds float64) {
	m.backendDialSeconds.WithLabelValues(cluster).Observe(seconds)
}

func (m *Metrics) AddBytes(cluster, direction string, n int64) {
	m.bytesTotal.WithLabelValues(cluster, direction).Add(float64(n))
}

func (m *Metrics) SetCertExpirySeconds(v float64) {
	m.certExpirySeconds.Set(v)
}

func (m *Metrics) SetDraining(draining bool) {
	if draining {
		m.draining.Set(1)
		return
	}
	m.draining.Set(0)
}

func (m *Metrics) SetNofileLimit(v float64) {
	m.nofileLimit.Set(v)
}

func (m *Metrics) SetRouteTableEntries(v float64) {
	m.routeTableEntries.Set(v)
}

func (m *Metrics) SetRouteTableLastSyncSeconds(v float64) {
	m.routeTableLastSyncSecs.Set(v)
}

func (m *Metrics) IncAuditError() {
	m.auditErrorsTotal.Inc()
}
