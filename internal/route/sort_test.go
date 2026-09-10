package route

import (
	"context"
	"testing"
)

// TestSortPoolerInfos_DeterministicWinner is the round-2 regression test
// for nondeterministic duplicate-hostname resolution: rebuildFromStore fed
// Table.Rebuild directly from informer.GetStore().List(), whose order is a
// Go map's iteration order — effectively random. Two Poolers racing for the
// same hostname could swap ownership on any rebuild, silently redirecting
// live traffic between tenants. Sorting by (namespace, name) first makes
// "first wins" a stable, explainable rule.
func TestSortPoolerInfos_DeterministicWinner(t *testing.T) {
	unsorted := []PoolerInfo{
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-b", Hostname: "tenant1.db.test", Cluster: "tenant1"},
		{Namespace: "pgproxy-e2e", Name: "tenant1-pooler-a", Hostname: "tenant1.db.test", Cluster: "tenant1"},
	}
	sortPoolerInfos(unsorted)

	if unsorted[0].Name != "tenant1-pooler-a" {
		t.Fatalf("after sortPoolerInfos, first entry = %q, want tenant1-pooler-a (alphabetically first)", unsorted[0].Name)
	}
}

func TestSortPoolerInfos_OrdersByNamespaceFirst(t *testing.T) {
	unsorted := []PoolerInfo{
		{Namespace: "z-namespace", Name: "a-pooler"},
		{Namespace: "a-namespace", Name: "z-pooler"},
	}
	sortPoolerInfos(unsorted)

	if unsorted[0].Namespace != "a-namespace" {
		t.Fatalf("after sortPoolerInfos, first entry's namespace = %q, want a-namespace", unsorted[0].Namespace)
	}
}

// TestInformerRouter_DuplicateHostname_StableAcrossRepeatedRebuilds drives
// the fix through the real informer path: two Poolers claiming the same
// hostname, rebuilt many times, must resolve to the same winner every time.
func TestInformerRouter_DuplicateHostname_StableAcrossRepeatedRebuilds(t *testing.T) {
	r, _ := newTestInformerRouter(
		newPooler("tenant1-pooler-b", "tenant1.db.test", "tenant1"),
		newPooler("tenant1-pooler-a", "tenant1.db.test", "tenant1"),
	)
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 20; i++ {
		r.rebuildFromStore()
		route, ok := r.Lookup("tenant1.db.test")
		if !ok {
			t.Fatalf("iteration %d: Lookup found nothing", i)
		}
		if route.Addr != "tenant1-pooler-a.pgproxy-e2e.svc:5432" {
			t.Fatalf("iteration %d: winner = %q, want tenant1-pooler-a (must stay stable across rebuilds)", i, route.Addr)
		}
	}
}
