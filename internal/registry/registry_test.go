package registry

import (
	"errors"
	"testing"
	"time"
)

func fixedLimits(maxConns, maxConnsPerCluster int) LimitsFunc {
	return func() Limits {
		return Limits{MaxConns: maxConns, MaxConnsPerCluster: maxConnsPerCluster}
	}
}

func testConn(id, cluster string) *Conn {
	return NewConn(ConnParams{
		ConnID:          id,
		Cluster:         cluster,
		SNI:             cluster + ".db.test",
		ClientIP:        "10.0.0.1",
		User:            "app_rw",
		Database:        "orders",
		ApplicationName: "psql",
		Backend:         "backend:5432",
		StartedAt:       time.Now(),
	})
}

func TestRegistry_AddRemoveSnapshotCount(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 0))

	if err := r.Add(testConn("c1", "tenant1")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Add(testConn("c2", "tenant2")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	total, perCluster := r.Count()
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if perCluster["tenant1"] != 1 || perCluster["tenant2"] != 1 {
		t.Errorf("perCluster = %v, want {tenant1:1, tenant2:1}", perCluster)
	}

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot() len = %d, want 2", len(snap))
	}

	r.Remove("c1")
	total, perCluster = r.Count()
	if total != 1 {
		t.Errorf("total after Remove = %d, want 1", total)
	}
	if perCluster["tenant1"] != 0 {
		t.Errorf("perCluster[tenant1] after Remove = %d, want 0", perCluster["tenant1"])
	}
}

func TestRegistry_CountZeroAfterAllRemoved(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 0))
	r.Add(testConn("c1", "tenant1"))
	r.Add(testConn("c2", "tenant1"))
	r.Add(testConn("c3", "tenant2"))

	r.Remove("c1")
	r.Remove("c2")
	r.Remove("c3")

	total, perCluster := r.Count()
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	for cluster, count := range perCluster {
		if count != 0 {
			t.Errorf("perCluster[%s] = %d, want 0", cluster, count)
		}
	}
	if len(r.Snapshot()) != 0 {
		t.Errorf("Snapshot() not empty after all removed")
	}
}

func TestRegistry_GlobalLimitExceeded(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(2, 0))

	if err := r.Add(testConn("c1", "tenant1")); err != nil {
		t.Fatalf("Add c1: %v", err)
	}
	if err := r.Add(testConn("c2", "tenant2")); err != nil {
		t.Fatalf("Add c2: %v", err)
	}
	err := r.Add(testConn("c3", "tenant1"))
	if !errors.Is(err, ErrTooManyConns) {
		t.Fatalf("Add c3 (over global limit) err = %v, want ErrTooManyConns", err)
	}
	total, _ := r.Count()
	if total != 2 {
		t.Errorf("total after rejected Add = %d, want 2 (rejected conn must not be registered)", total)
	}
}

func TestRegistry_PerClusterLimitExceeded(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 1))

	if err := r.Add(testConn("c1", "tenant1")); err != nil {
		t.Fatalf("Add c1: %v", err)
	}
	// Different cluster must not be affected by tenant1's limit.
	if err := r.Add(testConn("c2", "tenant2")); err != nil {
		t.Fatalf("Add c2 (different cluster): %v", err)
	}
	err := r.Add(testConn("c3", "tenant1"))
	if !errors.Is(err, ErrTooManyConns) {
		t.Fatalf("Add c3 (over per-cluster limit) err = %v, want ErrTooManyConns", err)
	}
}

// TestRegistry_TryAdmit_CountsPreAuthConnections is the round-2 regression
// test for the finding that pre-auth connections (TLS done, backend
// dialed, waiting on AuthenticationOk) counted toward nothing: overLimit
// checked only Count(), and Add only ran after auth completed. TryAdmit
// must reserve a slot immediately, before any registration.
func TestRegistry_TryAdmit_CountsPreAuthConnections(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 1))

	if !r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: first admission under the per-cluster limit was rejected")
	}
	// No Add has happened yet — Count() reflects only committed
	// connections — but a second admission for the same cluster must still
	// be rejected, because the first reservation is still outstanding.
	if r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: second admission was accepted even though the first (pre-auth) reservation is still outstanding")
	}
}

func TestRegistry_Release_FreesAnUnfulfilledReservation(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 1))

	if !r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: rejected under the limit")
	}
	r.Release("tenant1") // e.g. the backend never sent AuthenticationOk

	if !r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: rejected after the prior reservation was released")
	}
}

func TestRegistry_Add_ConsumesReservationWithoutDoubleCounting(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 1))

	if !r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: rejected under the limit")
	}
	if err := r.Add(testConn("c1", "tenant1")); err != nil {
		t.Fatalf("Add after TryAdmit: %v", err)
	}

	total, perCluster := r.Count()
	if total != 1 || perCluster["tenant1"] != 1 {
		t.Fatalf("Count() = total=%d perCluster=%v, want total=1 perCluster[tenant1]=1 (TryAdmit+Add must not double-count)", total, perCluster)
	}
	// The reservation was consumed by Add, so a second admission is judged
	// purely against the now-committed connection, not an extra phantom slot.
	if r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: admitted a second connection past the per-cluster limit of 1")
	}
}

func TestRegistry_TryAdmit_GlobalLimit(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(1, 0))

	if !r.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: first admission under the global limit was rejected")
	}
	if r.TryAdmit("tenant2") {
		t.Fatal("TryAdmit: admitted a second connection past the global limit, regardless of cluster")
	}
}
