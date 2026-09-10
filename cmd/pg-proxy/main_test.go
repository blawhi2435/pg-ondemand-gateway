package main

import (
	"os"
	"path/filepath"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/config"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/sysutil"
)

func gaugeValue(t *testing.T, families []*dto.MetricFamily, name string) (float64, bool) {
	t.Helper()
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if m.Gauge != nil {
				return m.Gauge.GetValue(), true
			}
		}
	}
	return 0, false
}

// TestApplyStartupGauges_SetsNofileLimit is the round-2 regression test for
// the metric documented in connection-lifecycle「啟動時提高 nofile 上限」
// (Scenario: pgproxy_nofile_limit 反映最終值) — RaiseNofileLimit's result
// was previously discarded in run(), so the gauge stayed at 0 forever.
func TestApplyStartupGauges_SetsNofileLimit(t *testing.T) {
	met := metrics.New()
	applyStartupGauges(met, sysutil.NofileResult{Before: 1024, After: 1048576})

	families, err := met.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got, ok := gaugeValue(t, families, "pgproxy_nofile_limit")
	if !ok {
		t.Fatal("pgproxy_nofile_limit series not found")
	}
	if got != 1048576 {
		t.Errorf("pgproxy_nofile_limit = %v, want 1048576", got)
	}
}

func TestLimitsFrom_ReadsCurrentConfigOnEachCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "limits:\n  maxConns: 5\n  maxConnsPerCluster: 2\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	limits := limitsFrom(store)

	got := limits()
	if got.MaxConns != 5 || got.MaxConnsPerCluster != 2 {
		t.Fatalf("limits() = %+v, want {MaxConns:5 MaxConnsPerCluster:2}", got)
	}

	updated := "limits:\n  maxConns: 50\n  maxConnsPerCluster: 20\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if _, err := store.ReloadNow(); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}

	// limitsFrom must read Current() fresh on every call, not capture a
	// snapshot at construction time — that's what makes hot reload work.
	got = limits()
	if got.MaxConns != 50 || got.MaxConnsPerCluster != 20 {
		t.Fatalf("limits() after reload = %+v, want {MaxConns:50 MaxConnsPerCluster:20} (limitsFrom must not cache)", got)
	}
}

// TestCancelLimitsFrom_DisablesPerIPWhenProxyProtocolOff is the round-3
// regression test for task 28.1/28.2: in this deployment's default
// topology every connection shares APISIX's one pod IP unless PROXY
// protocol is enabled and trusted, so a configured per-IP cancel limit
// must be forced off (MaxPerIP<=0) until PROXY protocol actually recovers
// real client addresses — otherwise "N per IP" silently becomes a global
// budget for the whole fleet.
func TestCancelLimitsFrom_DisablesPerIPWhenProxyProtocolOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "limits:\n  cancel:\n    maxPerIP: 5\n    maxInFlight: 50\nproxyProtocol:\n  enabled: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	limits := cancelLimitsFrom(store)()
	if limits.MaxPerIP > 0 {
		t.Fatalf("MaxPerIP = %d with proxyProtocol.enabled=false, want <= 0 (disabled)", limits.MaxPerIP)
	}
	if limits.MaxInFlight != 50 {
		t.Errorf("MaxInFlight = %d, want the configured 50 — only the per-IP check should be gated", limits.MaxInFlight)
	}
}

// TestCancelLimitsFrom_KeepsPerIPWhenProxyProtocolOn is the paired case:
// once PROXY protocol is enabled (and therefore trusted, per validate's
// fail-closed rule requiring a non-empty trustedCIDRs), the configured
// per-IP cap must be honored, not silently zeroed.
func TestCancelLimitsFrom_KeepsPerIPWhenProxyProtocolOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "limits:\n  cancel:\n    maxPerIP: 5\n    maxInFlight: 50\nproxyProtocol:\n  enabled: true\n  trustedCIDRs: [\"10.0.0.0/8\"]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	limits := cancelLimitsFrom(store)()
	if limits.MaxPerIP != 5 {
		t.Errorf("MaxPerIP = %d with proxyProtocol.enabled=true, want the configured 5", limits.MaxPerIP)
	}
}

// TestCancelLimitsFrom_ReadsCurrentConfigOnEachCall proves the gate is
// hot-reloadable like every other limit in design §8.1 — toggling
// proxyProtocol.enabled via ConfigMap must take effect without a restart.
func TestCancelLimitsFrom_ReadsCurrentConfigOnEachCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "limits:\n  cancel:\n    maxPerIP: 5\nproxyProtocol:\n  enabled: false\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	limits := cancelLimitsFrom(store)

	if got := limits().MaxPerIP; got > 0 {
		t.Fatalf("MaxPerIP = %d before enabling PROXY protocol, want <= 0", got)
	}

	updated := "limits:\n  cancel:\n    maxPerIP: 5\nproxyProtocol:\n  enabled: true\n  trustedCIDRs: [\"10.0.0.0/8\"]\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if _, err := store.ReloadNow(); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}

	if got := limits().MaxPerIP; got != 5 {
		t.Fatalf("MaxPerIP after enabling PROXY protocol = %d, want 5 (cancelLimitsFrom must not cache)", got)
	}
}
