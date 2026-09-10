package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

const validConfig = `
listen: ":5432"
metricsListen: ":9090"
limits:
  maxConns: 5000
  maxConnsPerCluster: 500
timeouts:
  handshake: 10s
  backendDial: 5s
  idle: 300s
shutdown:
  drainTimeout: 300s
`

func TestLoad_ParsesValidConfig(t *testing.T) {
	path := writeFile(t, t.TempDir(), validConfig)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":5432" {
		t.Errorf("Listen = %q, want :5432", cfg.Listen)
	}
	if cfg.Limits.MaxConns != 5000 {
		t.Errorf("Limits.MaxConns = %d, want 5000", cfg.Limits.MaxConns)
	}
	if cfg.Timeouts.Idle.Duration() != 300*time.Second {
		t.Errorf("Timeouts.Idle = %v, want 300s", cfg.Timeouts.Idle.Duration())
	}
}

// TestLoad_MinimalConfigMapGetsDesignDefaults is the round-2 regression
// test for the security review's finding that a ConfigMap omitting the
// timeouts/limits block parsed clean and silently disabled every resource
// guard (limits.maxConns=0 means no limit at all, timeouts.idle=0 means
// half-open connections are never reaped, timeouts.handshake=0 is an
// already-expired deadline that accepts nothing). A minimal ConfigMap must
// still get the design §8.1 defaults, not zeroes.
func TestLoad_MinimalConfigMapGetsDesignDefaults(t *testing.T) {
	path := writeFile(t, t.TempDir(), "route:\n  namespace: pgproxy-e2e\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen == "" {
		t.Error("Listen defaulted to empty (binds a random port silently)")
	}
	if cfg.MetricsListen == "" {
		t.Error("MetricsListen defaulted to empty")
	}
	if cfg.Limits.MaxConns == 0 {
		t.Error("Limits.MaxConns defaulted to 0 (no connection limit at all)")
	}
	if cfg.Limits.MaxConnsPerCluster == 0 {
		t.Error("Limits.MaxConnsPerCluster defaulted to 0 (no per-cluster limit at all)")
	}
	if cfg.Timeouts.Handshake.Duration() <= 0 {
		t.Error("Timeouts.Handshake defaulted to <= 0 (an already-expired deadline)")
	}
	if cfg.Timeouts.BackendDial.Duration() <= 0 {
		t.Error("Timeouts.BackendDial defaulted to <= 0")
	}
	if cfg.Timeouts.Idle.Duration() <= 0 {
		t.Error("Timeouts.Idle defaulted to <= 0 (half-open connections never reaped)")
	}
	if cfg.Shutdown.DrainTimeout.Duration() <= 0 {
		t.Error("Shutdown.DrainTimeout defaulted to <= 0 (drain degrades to instant force-close)")
	}
	if cfg.TLS.ReloadInterval.Duration() <= 0 {
		t.Error("TLS.ReloadInterval defaulted to <= 0")
	}
}

func TestLoad_ExplicitValuesAreNotOverriddenByDefaults(t *testing.T) {
	path := writeFile(t, t.TempDir(), `
limits:
  maxConns: 20
  maxConnsPerCluster: 10
timeouts:
  idle: 45s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.MaxConns != 20 {
		t.Errorf("Limits.MaxConns = %d, want the explicit 20 (test env values must be reachable, not silently replaced by the default)", cfg.Limits.MaxConns)
	}
	if cfg.Limits.MaxConnsPerCluster != 10 {
		t.Errorf("Limits.MaxConnsPerCluster = %d, want the explicit 10", cfg.Limits.MaxConnsPerCluster)
	}
	if cfg.Timeouts.Idle.Duration() != 45*time.Second {
		t.Errorf("Timeouts.Idle = %v, want the explicit 45s", cfg.Timeouts.Idle.Duration())
	}
}

// TestLoad_MinimalConfigMapGetsCancelLimitDefaults is the round-3 regression
// test for task 28.6: the cancel abuse-protection bounds were previously
// hardcoded constants in main.go; they now live in the ConfigMap, hot-
// reloadable like every other limit in design §8.1, and a minimal
// ConfigMap must still get sane defaults rather than zero (which would
// disable the in-flight cap entirely).
func TestLoad_MinimalConfigMapGetsCancelLimitDefaults(t *testing.T) {
	path := writeFile(t, t.TempDir(), "route:\n  namespace: pgproxy-e2e\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.Cancel.Window.Duration() <= 0 {
		t.Error("Limits.Cancel.Window defaulted to <= 0")
	}
	if cfg.Limits.Cancel.MaxInFlight <= 0 {
		t.Error("Limits.Cancel.MaxInFlight defaulted to <= 0 (disables the in-flight cap entirely)")
	}
}

func TestLoad_ExplicitCancelLimitsAreNotOverridden(t *testing.T) {
	path := writeFile(t, t.TempDir(), `
limits:
  cancel:
    window: 30s
    maxPerIP: 7
    maxInFlight: 20
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.Cancel.Window.Duration() != 30*time.Second {
		t.Errorf("Limits.Cancel.Window = %v, want 30s", cfg.Limits.Cancel.Window.Duration())
	}
	if cfg.Limits.Cancel.MaxPerIP != 7 {
		t.Errorf("Limits.Cancel.MaxPerIP = %d, want 7", cfg.Limits.Cancel.MaxPerIP)
	}
	if cfg.Limits.Cancel.MaxInFlight != 20 {
		t.Errorf("Limits.Cancel.MaxInFlight = %d, want 20", cfg.Limits.Cancel.MaxInFlight)
	}
}

func TestLoad_RejectsInvalidTrustedCIDR(t *testing.T) {
	bad := `
listen: ":5432"
proxyProtocol:
  enabled: true
  trustedCIDRs: ["not-a-cidr"]
`
	path := writeFile(t, t.TempDir(), bad)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for an invalid trustedCIDRs entry, got nil")
	}
}

func TestLoad_RejectsNegativeTimeout(t *testing.T) {
	bad := `
listen: ":5432"
timeouts:
  idle: -5s
`
	path := writeFile(t, t.TempDir(), bad)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for negative timeout, got nil")
	}
}

func TestStore_ReloadNow_HashUnchangedDoesNotReplace(t *testing.T) {
	path := writeFile(t, t.TempDir(), validConfig)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := store.Current()

	changed, err := store.ReloadNow()
	if err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if changed {
		t.Error("ReloadNow reported changed=true when file content did not change")
	}
	if store.Current() != before {
		t.Error("Current() pointer changed even though the file content is identical")
	}
}

func TestStore_ReloadNow_ChangedContentReplaces(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, validConfig)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := store.Current()

	updated := `
listen: ":5432"
metricsListen: ":9090"
limits:
  maxConns: 20
  maxConnsPerCluster: 10
timeouts:
  handshake: 10s
  backendDial: 5s
  idle: 300s
shutdown:
  drainTimeout: 300s
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	changed, err := store.ReloadNow()
	if err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if !changed {
		t.Fatal("ReloadNow reported changed=false after the file content changed")
	}
	if store.Current() == before {
		t.Error("Current() pointer did not change after a content change")
	}
	if store.Current().Limits.MaxConnsPerCluster != 10 {
		t.Errorf("Limits.MaxConnsPerCluster = %d, want 10", store.Current().Limits.MaxConnsPerCluster)
	}
}

func TestStore_ReloadNow_InvalidConfigKeepsOldValue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, validConfig)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := store.Current()

	invalid := `
listen: ":5432"
timeouts:
  idle: -1s
`
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	changed, err := store.ReloadNow()
	if err == nil {
		t.Fatal("ReloadNow: want error for invalid config, got nil")
	}
	if changed {
		t.Error("ReloadNow reported changed=true for a config that failed validation")
	}
	if store.Current() != before {
		t.Error("Current() must keep serving the last-good config after an invalid reload")
	}
}

// TestLoad_RejectsProxyProtocolEnabledWithEmptyTrustedCIDRs is the round-3
// regression test for task 28.4/28.5: enabling PROXY protocol with no
// trustedCIDRs entries fails closed at runtime — isTrustedProxySource
// trusts nothing and every single connection gets rejected — but that
// footgun previously only surfaced once traffic hit the pod. It must be
// caught at config-load time instead, where a human reviewing `kubectl
// apply` output (or a CI validation step) can see it before it takes
// production down.
func TestLoad_RejectsProxyProtocolEnabledWithEmptyTrustedCIDRs(t *testing.T) {
	bad := `
listen: ":5432"
proxyProtocol:
  enabled: true
`
	path := writeFile(t, t.TempDir(), bad)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for proxyProtocol.enabled=true with no trustedCIDRs, got nil")
	}
}

func TestLoad_ProxyProtocolDisabledAllowsEmptyTrustedCIDRs(t *testing.T) {
	path := writeFile(t, t.TempDir(), "proxyProtocol:\n  enabled: false\n")

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: proxyProtocol.enabled=false with no trustedCIDRs should be valid, got: %v", err)
	}
}

// TestLoad_RejectsNegativeCancelMaxPerIP is the round-3 regression test for
// task 30.3/30.4: a negative limits.cancel.maxPerIP or maxInFlight must be
// rejected at load time. maxInFlight is especially dangerous silently:
// CancelRateLimiter's Allow treats "<= 0" as "disabled", so a stray
// negative number (e.g. from a bad hot-reload edit) would silently turn
// off the one topology-independent ceiling CancelRequest abuse protection
// has left.
func TestLoad_RejectsNegativeCancelMaxPerIP(t *testing.T) {
	bad := `
limits:
  cancel:
    maxPerIP: -1
`
	path := writeFile(t, t.TempDir(), bad)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for negative limits.cancel.maxPerIP, got nil")
	}
}

func TestLoad_RejectsNegativeCancelMaxInFlight(t *testing.T) {
	bad := `
limits:
  cancel:
    maxInFlight: -1
`
	path := writeFile(t, t.TempDir(), bad)

	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for negative limits.cancel.maxInFlight, got nil")
	}
}
