package route

import (
	"sync"
	"testing"
)

// fakeEventRecorder captures emitted events for assertions instead of
// talking to a real k8s API server.
type fakeEventRecorder struct {
	mu     sync.Mutex
	events []event
}

type event struct {
	namespace, name, reason, message string
}

func (f *fakeEventRecorder) Warn(namespace, name, reason, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event{namespace, name, reason, message})
}

func (f *fakeEventRecorder) reasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		out = append(out, e.reason)
	}
	return out
}

func TestTable_Rebuild_WithAnnotationEntersTable(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)

	route, ok := tbl.Lookup("tenant1.db.test")
	if !ok {
		t.Fatal("Lookup(tenant1.db.test): not found")
	}
	if route.Cluster != "tenant1" {
		t.Errorf("Cluster = %q, want tenant1", route.Cluster)
	}
	if route.Addr != "tenant1-pooler.pgproxy-e2e.svc:5432" {
		t.Errorf("Addr = %q, want tenant1-pooler.pgproxy-e2e.svc:5432", route.Addr)
	}
}

func TestTable_Rebuild_WithoutAnnotationSkipped(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "no-annotation-pooler", Hostname: "", Cluster: ""},
	}, rec)

	if _, ok := tbl.Lookup(""); ok {
		t.Fatal("empty hostname must never be looked up successfully")
	}
	if tbl.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", tbl.Len())
	}
	if len(rec.reasons()) != 0 {
		t.Fatalf("expected no events for a Pooler with no hostname annotation, got %v", rec.reasons())
	}
}

func TestTable_Rebuild_MultiLabelHostnameRejected(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "bad-pooler", Hostname: "a.b.db.test", Cluster: "tenant1"},
	}, rec)

	if _, ok := tbl.Lookup("a.b.db.test"); ok {
		t.Fatal("multi-label hostname must not enter the route table")
	}
	reasons := rec.reasons()
	if len(reasons) != 1 || reasons[0] != "InvalidHostname" {
		t.Fatalf("reasons = %v, want [InvalidHostname]", reasons)
	}
}

func TestTable_Rebuild_DuplicateHostnameRejectsLater(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-a", Hostname: "tenant1.db.test", Cluster: "tenant1"},
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-b", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)

	route, ok := tbl.Lookup("tenant1.db.test")
	if !ok {
		t.Fatal("Lookup(tenant1.db.test): not found")
	}
	if route.Addr != "tenant1-pooler-a.pgproxy-e2e.svc:5432" {
		t.Errorf("Addr = %q, want the first-seen Pooler to win", route.Addr)
	}
	reasons := rec.reasons()
	if len(reasons) != 1 || reasons[0] != "DuplicateHostname" {
		t.Fatalf("reasons = %v, want [DuplicateHostname]", reasons)
	}
}

func TestTable_Rebuild_FormerWinnerDeletedLatterTakesOver(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-a", Hostname: "tenant1.db.test", Cluster: "tenant1"},
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-b", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)

	// tenant1-pooler-a is deleted; the next rebuild only sees pooler-b.
	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-b", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)

	route, ok := tbl.Lookup("tenant1.db.test")
	if !ok {
		t.Fatal("Lookup(tenant1.db.test): not found after former winner deleted")
	}
	if route.Addr != "tenant1-pooler-b.pgproxy-e2e.svc:5432" {
		t.Errorf("Addr = %q, want tenant1-pooler-b to take over", route.Addr)
	}
}

// TestTable_Rebuild_MissingClusterAnnotationRejected is the round-2
// regression test: a Pooler with a valid hostname but no
// pg-proxy.internal/cluster annotation was previously admitted with
// Cluster="". That empty string then becomes the audit log's cluster
// field, the cluster label on every metric for that connection, and phase
// 2's sole input for the Redis policy key — a typo in one annotation would
// otherwise produce a silently unattributable tenant instead of a visible
// error.
func TestTable_Rebuild_MissingClusterAnnotationRejected(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "orphan-pooler", Hostname: "tenant1.db.test", Cluster: ""},
	}, rec)

	if _, ok := tbl.Lookup("tenant1.db.test"); ok {
		t.Fatal("a Pooler with no cluster annotation must not enter the route table")
	}
	reasons := rec.reasons()
	if len(reasons) != 1 || reasons[0] != "MissingCluster" {
		t.Fatalf("reasons = %v, want [MissingCluster]", reasons)
	}
}

// TestTable_Lookup_CaseInsensitive is the round-2 regression test for RFC
// 6066: TLS SNI is case-insensitive, so a client sending "Tenant1.db.test"
// must resolve the same route as "tenant1.db.test".
func TestTable_Lookup_CaseInsensitive(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)

	for _, sni := range []string{"tenant1.db.test", "Tenant1.db.test", "TENANT1.DB.TEST"} {
		route, ok := tbl.Lookup(sni)
		if !ok {
			t.Errorf("Lookup(%q): not found", sni)
			continue
		}
		if route.Cluster != "tenant1" {
			t.Errorf("Lookup(%q).Cluster = %q, want tenant1", sni, route.Cluster)
		}
	}
}

func TestTable_Rebuild_IsFullReplaceNotInPlace(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	tbl.Rebuild([]PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}, rec)
	if tbl.Len() != 1 {
		t.Fatalf("Len() after first rebuild = %d, want 1", tbl.Len())
	}

	// Second rebuild with an empty list must remove the earlier entry.
	tbl.Rebuild(nil, rec)
	if tbl.Len() != 0 {
		t.Fatalf("Len() after empty rebuild = %d, want 0 (rebuild must fully replace)", tbl.Len())
	}
	if _, ok := tbl.Lookup("tenant1.db.test"); ok {
		t.Fatal("stale entry survived a full rebuild")
	}
}
