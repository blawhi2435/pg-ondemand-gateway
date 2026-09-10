package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewServer_HasTimeouts is the round-2 regression test for the
// Slowloris finding (gosec G112): a plain &http.Server{} with no
// ReadHeaderTimeout lets a handful of connections that open and never
// finish sending headers tie up goroutines indefinitely — reachable from
// anywhere on the pod network if metricsListen binds 0.0.0.0, and taking
// down /healthz//readyz has knock-on effects on the pod's lifecycle.
func TestNewServer_HasTimeouts(t *testing.T) {
	srv := NewServer(":0", New(), NewProber(func() bool { return true }))

	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is unset — vulnerable to Slowloris (gosec G112)")
	}
	if srv.ReadTimeout <= 0 {
		t.Error("ReadTimeout is unset")
	}
	if srv.WriteTimeout <= 0 {
		t.Error("WriteTimeout is unset")
	}
	if srv.ReadHeaderTimeout > 30*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, unreasonably long for a metrics endpoint", srv.ReadHeaderTimeout)
	}
}

func TestNewServer_RoutesAllThreeEndpoints(t *testing.T) {
	m := New()
	m.IncActive("tenant1")
	prober := NewProber(func() bool { return true })

	srv := NewServer(":0", m, prober)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp2.StatusCode)
	}

	resp3, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("/readyz status = %d, want 200", resp3.StatusCode)
	}
}

func TestNewServer_MetricsBodyContainsRegisteredSeries(t *testing.T) {
	m := New()
	m.IncActive("tenant1")
	prober := NewProber(func() bool { return true })

	srv := NewServer(":0", m, prober)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "pgproxy_connections_active") {
		t.Errorf("/metrics body missing pgproxy_connections_active:\n%s", body)
	}
}
