package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func gaugeValue(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				}
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

// TestMetrics_ConnectionsActive_IncDecAndZero is a unit test of the gauge
// mechanics only — it proves prometheus.GaugeVec can count, nothing more.
// It does NOT exercise the spec's actual leak-detection scenario
// (pgproxy-observability「連線全部結束後歸零」), which requires driving a real
// connection through Handler.HandleConn end to end and observing IncActive/
// DecActive as a side effect of that — see
// TestHandleConn_AuthorizerBeforeDial_RegistryAfterAuthOk in
// internal/server, which asserts this gauge returns to 0 after a real
// connection closes.
func TestMetrics_ConnectionsActive_IncDecAndZero(t *testing.T) {
	m := New()

	m.IncActive("tenant1")
	m.IncActive("tenant1")
	m.IncActive("tenant1")
	if got := gaugeValue(t, m, "pgproxy_connections_active", map[string]string{"cluster": "tenant1"}); got != 3 {
		t.Fatalf("active after 3 IncActive = %v, want 3", got)
	}

	m.DecActive("tenant1")
	m.DecActive("tenant1")
	m.DecActive("tenant1")
	if got := gaugeValue(t, m, "pgproxy_connections_active", map[string]string{"cluster": "tenant1"}); got != 0 {
		t.Fatalf("active after 3 DecActive = %v, want 0", got)
	}
}

func TestMetrics_HandshakeErrorsTotal_ClassifiedByReason(t *testing.T) {
	m := New()

	m.IncHandshakeError("unknown_sni")
	m.IncHandshakeError("unknown_sni")
	m.IncHandshakeError("bad_startup")

	if got := gaugeValue(t, m, "pgproxy_handshake_errors_total", map[string]string{"reason": "unknown_sni"}); got != 2 {
		t.Fatalf("unknown_sni count = %v, want 2", got)
	}
	if got := gaugeValue(t, m, "pgproxy_handshake_errors_total", map[string]string{"reason": "bad_startup"}); got != 1 {
		t.Fatalf("bad_startup count = %v, want 1", got)
	}
}

func TestMetrics_ConnectionsTotal_ByClusterAndResult(t *testing.T) {
	m := New()

	m.IncConnectionsTotal("tenant1", "ok")
	m.IncConnectionsTotal("tenant1", "limit_exceeded")

	if got := gaugeValue(t, m, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "ok"}); got != 1 {
		t.Fatalf("ok count = %v, want 1", got)
	}
	if got := gaugeValue(t, m, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "limit_exceeded"}); got != 1 {
		t.Fatalf("limit_exceeded count = %v, want 1", got)
	}
}

func TestMetrics_GaugesRegistered(t *testing.T) {
	m := New()

	m.SetDraining(true)
	m.SetNofileLimit(1048576)
	m.SetRouteTableEntries(2)
	m.SetRouteTableLastSyncSeconds(0.5)
	m.SetCertExpirySeconds(1234)

	cases := map[string]float64{
		"pgproxy_draining":                      1,
		"pgproxy_nofile_limit":                  1048576,
		"pgproxy_route_table_entries":           2,
		"pgproxy_route_table_last_sync_seconds": 0.5,
		"pgproxy_cert_expiry_seconds":           1234,
	}
	for name, want := range cases {
		if got := gaugeValue(t, m, name, nil); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestMetrics_AllMetricsAppearInGather(t *testing.T) {
	m := New()
	m.IncActive("tenant1")
	m.IncConnectionsTotal("tenant1", "ok")
	m.IncHandshakeError("unknown_sni")
	m.ObserveBackendDialSeconds("tenant1", 0.01)
	m.AddBytes("tenant1", "in", 100)
	m.SetCertExpirySeconds(1)
	m.SetDraining(false)
	m.SetNofileLimit(1)
	m.SetRouteTableEntries(1)
	m.SetRouteTableLastSyncSeconds(1)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, fam := range families {
		names[fam.GetName()] = true
	}
	for _, want := range []string{
		"pgproxy_connections_active", "pgproxy_connections_total", "pgproxy_handshake_errors_total",
		"pgproxy_backend_dial_seconds", "pgproxy_bytes_total", "pgproxy_cert_expiry_seconds",
		"pgproxy_draining", "pgproxy_nofile_limit", "pgproxy_route_table_entries",
		"pgproxy_route_table_last_sync_seconds",
	} {
		if !names[want] {
			t.Errorf("metric %s not registered", want)
		}
	}
}

// TestLabelConstants_MatchDocumentedDomains is the round-3 regression test
// for task 28.7: the connection-outcome, handshake-error-reason, and
// byte-direction label values previously existed only as bare string
// literals scattered across internal/server (23 call sites) with nothing
// to catch a typo'd label creating a silent new time series instead of
// matching an existing one. Named constants, exercised here, are the
// single source of truth every caller must reference.
func TestLabelConstants_MatchDocumentedDomains(t *testing.T) {
	wantResults := []string{ResultOK, ResultDenied, ResultLimitExceeded, ResultBackendError}
	for _, r := range wantResults {
		if r == "" {
			t.Errorf("a ResultXxx constant is empty")
		}
	}
	if ResultOK != "ok" || ResultDenied != "denied" || ResultLimitExceeded != "limit_exceeded" || ResultBackendError != "backend_error" {
		t.Errorf("Result constants = %q,%q,%q,%q, want ok,denied,limit_exceeded,backend_error",
			ResultOK, ResultDenied, ResultLimitExceeded, ResultBackendError)
	}

	if DirectionIn != "in" || DirectionOut != "out" {
		t.Errorf("Direction constants = %q,%q, want in,out", DirectionIn, DirectionOut)
	}

	wantReasons := map[string]string{
		"ReasonBadStartup":           ReasonBadStartup,
		"ReasonTLSError":             ReasonTLSError,
		"ReasonUnknownSNI":           ReasonUnknownSNI,
		"ReasonPlaintextRejected":    ReasonPlaintextRejected,
		"ReasonUntrustedProxySource": ReasonUntrustedProxySource,
		"ReasonBadProxyHeader":       ReasonBadProxyHeader,
		"ReasonCancelRateLimited":    ReasonCancelRateLimited,
	}
	seen := map[string]bool{}
	for name, v := range wantReasons {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
		if seen[v] {
			t.Errorf("%s = %q collides with another reason constant", name, v)
		}
		seen[v] = true
	}
}
