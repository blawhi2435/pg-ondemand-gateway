package route

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "pgproxy-e2e"

var poolerListKinds = map[schema.GroupVersionResource]string{
	poolerGVR: "PoolerList",
}

func newPooler(name, hostname, cluster string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("postgresql.cnpg.io/v1")
	u.SetKind("Pooler")
	u.SetName(name)
	u.SetNamespace(testNamespace)
	annotations := map[string]string{}
	if hostname != "" {
		annotations["pg-proxy.internal/hostname"] = hostname
	}
	if cluster != "" {
		annotations["pg-proxy.internal/cluster"] = cluster
	}
	u.SetAnnotations(annotations)
	return u
}

func newTestInformerRouter(objs ...runtime.Object) (*InformerRouter, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, poolerListKinds, objs...)
	cfg := InformerConfig{
		Namespace:          testNamespace,
		HostnameAnnotation: "pg-proxy.internal/hostname",
		ClusterAnnotation:  "pg-proxy.internal/cluster",
		ResyncPeriod:       time.Second,
	}
	return NewInformerRouter(client, cfg, &fakeEventRecorder{}), client
}

func TestInformerRouter_NotReadyBeforeStart(t *testing.T) {
	r, _ := newTestInformerRouter(newPooler("tenant1-pooler", "tenant1.db.test", "tenant1"))

	if r.Ready() {
		t.Fatal("Ready() = true before Start; want false until cache sync completes")
	}
}

func TestInformerRouter_ReadyAfterStart(t *testing.T) {
	r, _ := newTestInformerRouter(newPooler("tenant1-pooler", "tenant1.db.test", "tenant1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !r.Ready() {
		t.Fatal("Ready() = false after Start returned; want true")
	}
	route, ok := r.Lookup("tenant1.db.test")
	if !ok || route.Cluster != "tenant1" {
		t.Fatalf("Lookup(tenant1.db.test) = %+v, %v; want cluster=tenant1", route, ok)
	}
}

// TestInformerRouter_LastSyncSeconds is the round-2 regression test for the
// informer's only stall-detection signal — previously untested entirely.
// sni-routing Scenario「執行中 API server 中斷」requires that pg-proxy keeps
// serving from local cache *and* that this metric keeps growing "以供告警";
// this test covers the first half directly and the growth behavior, which
// is what a stuck watch would fail to produce.
func TestInformerRouter_LastSyncSeconds(t *testing.T) {
	r, _ := newTestInformerRouter(newPooler("tenant1-pooler", "tenant1.db.test", "tenant1"))

	if got := r.LastSyncSeconds(); got != 0 {
		t.Fatalf("LastSyncSeconds() before Start = %v, want 0", got)
	}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := r.LastSyncSeconds()
	if first < 0 {
		t.Fatalf("LastSyncSeconds() after Start = %v, want >= 0", first)
	}

	time.Sleep(50 * time.Millisecond)
	if got := r.LastSyncSeconds(); got <= first {
		t.Errorf("LastSyncSeconds() did not grow over elapsed time with no further sync (got %v, was %v) — it must reflect staleness even when nothing has changed", got, first)
	}

	// Routing keeps working from the local cache throughout.
	if _, ok := r.Lookup("tenant1.db.test"); !ok {
		t.Error("routing stopped working from local cache")
	}
}

func TestInformerRouter_InitialListFailureReturnsError(t *testing.T) {
	r, client := newTestInformerRouter()
	client.PrependReactor("list", "poolers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	err := r.Start(context.Background())
	if err == nil {
		t.Fatal("Start: want error when the initial List fails, got nil")
	}
	if r.Ready() {
		t.Fatal("Ready() = true after a failed initial List; want false")
	}
}

func TestInformerRouter_AddPoolerIncreasesTable(t *testing.T) {
	r, client := newTestInformerRouter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.Len() != 0 {
		t.Fatalf("Len() = %d before adding a Pooler, want 0", r.Len())
	}

	newPoolerObj := newPooler("tenant2-pooler", "tenant2.db.test", "tenant2")
	if _, err := client.Resource(poolerGVR).Namespace(testNamespace).Create(ctx, newPoolerObj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create pooler: %v", err)
	}

	waitFor(t, func() bool { return r.Len() == 1 })
	if _, ok := r.Lookup("tenant2.db.test"); !ok {
		t.Fatal("newly added Pooler did not become routable")
	}
}

func TestInformerRouter_DeletePoolerDecreasesTable(t *testing.T) {
	r, client := newTestInformerRouter(newPooler("tenant1-pooler", "tenant1.db.test", "tenant1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return r.Len() == 1 })

	if err := client.Resource(poolerGVR).Namespace(testNamespace).Delete(ctx, "tenant1-pooler", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete pooler: %v", err)
	}

	waitFor(t, func() bool { return r.Len() == 0 })
	if _, ok := r.Lookup("tenant1.db.test"); ok {
		t.Fatal("deleted Pooler is still routable")
	}
}

// waitFor polls cond until it's true or fails the test after a short bound —
// informer event delivery is asynchronous even against a fake client.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
