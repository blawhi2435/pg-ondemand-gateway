package route

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// poolerGVR identifies the CNPG Pooler CRD. pg-proxy only ever reads
// metadata (name/namespace/annotations) through it, so a dynamic client is
// enough — a typed client would pull in the whole CNPG Go module (design §7.2).
var poolerGVR = schema.GroupVersionResource{
	Group:    "postgresql.cnpg.io",
	Version:  "v1",
	Resource: "poolers",
}

// InformerConfig configures where and how the Pooler informer watches.
type InformerConfig struct {
	Namespace          string
	HostnameAnnotation string
	ClusterAnnotation  string
	ResyncPeriod       time.Duration
}

// InformerRouter is seam A's production implementation: a Router backed by
// a dynamic List-then-Watch informer over Pooler CRs (design §7).
type InformerRouter struct {
	table  *Table
	events EventRecorder
	client dynamic.Interface
	cfg    InformerConfig

	informer cache.SharedIndexInformer
	synced   atomic.Bool
	lastSync atomic.Int64 // UnixNano of the last successful rebuild
}

// NewInformerRouter builds an InformerRouter. Call Start before using it as
// a Router — until then Lookup always misses and Ready reports false.
func NewInformerRouter(client dynamic.Interface, cfg InformerConfig, events EventRecorder) *InformerRouter {
	return &InformerRouter{
		table:  NewTable(),
		events: events,
		client: client,
		cfg:    cfg,
	}
}

// Start performs the initial List against the API server, then launches the
// watch loop and blocks until the local cache has synced. Per design §7.4,
// a failed initial List is returned as an error so the caller can crash the
// process rather than run with an empty, silently-broken route table.
func (r *InformerRouter) Start(ctx context.Context) error {
	if _, err := r.client.Resource(poolerGVR).Namespace(r.cfg.Namespace).List(ctx, metav1.ListOptions{}); err != nil {
		return fmt.Errorf("route: initial List of poolers failed: %w", err)
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		r.client, r.cfg.ResyncPeriod, r.cfg.Namespace, nil)
	r.informer = factory.ForResource(poolerGVR).Informer()

	handler := func(interface{}) { r.rebuildFromStore() }
	if _, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handler,
		UpdateFunc: func(_, _ interface{}) { r.rebuildFromStore() },
		DeleteFunc: handler,
	}); err != nil {
		return fmt.Errorf("route: register informer event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), r.informer.HasSynced) {
		return fmt.Errorf("route: cache sync did not complete before context was cancelled")
	}

	r.rebuildFromStore()
	r.synced.Store(true)
	return nil
}

// Lookup implements Router.
func (r *InformerRouter) Lookup(sni string) (Route, bool) {
	return r.table.Lookup(sni)
}

// Ready reports whether the initial cache sync has completed. Callers use
// this to gate opening the :5432 listener and to answer /readyz.
func (r *InformerRouter) Ready() bool {
	return r.synced.Load()
}

// Len returns the current route table size, for pgproxy_route_table_entries.
func (r *InformerRouter) Len() int {
	return r.table.Len()
}

// LastSyncSeconds returns how many seconds have elapsed since the last
// successful rebuild. It grows without bound if the watch has silently
// died — the metric this feeds is the only signal for that failure mode.
func (r *InformerRouter) LastSyncSeconds() float64 {
	last := r.lastSync.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last)).Seconds()
}

// rebuildFromStore reads every Pooler currently in the informer's local
// cache and feeds it through Table.Rebuild.
func (r *InformerRouter) rebuildFromStore() {
	objs := r.informer.GetStore().List()
	infos := make([]PoolerInfo, 0, len(objs))
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		infos = append(infos, r.toPoolerInfo(u))
	}
	// informer.GetStore().List() order is a Go map's iteration order —
	// effectively random. Table.Rebuild resolves a hostname collision by
	// "first wins", so without a stable order here, two Poolers racing for
	// the same hostname could swap ownership on any rebuild, silently
	// redirecting live traffic between tenants.
	sortPoolerInfos(infos)
	r.table.Rebuild(infos, r.events)
	r.lastSync.Store(time.Now().UnixNano())
}

// sortPoolerInfos orders infos by (namespace, name) so "first wins" is a
// deterministic, explainable rule rather than depending on map iteration
// order.
func sortPoolerInfos(infos []PoolerInfo) {
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Namespace != infos[j].Namespace {
			return infos[i].Namespace < infos[j].Namespace
		}
		return infos[i].Name < infos[j].Name
	})
}

// toPoolerInfo extracts the fields Rebuild needs from a Pooler's metadata.
func (r *InformerRouter) toPoolerInfo(u *unstructured.Unstructured) PoolerInfo {
	annotations := u.GetAnnotations()
	return PoolerInfo{
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		Hostname:  annotations[r.cfg.HostnameAnnotation],
		Cluster:   annotations[r.cfg.ClusterAnnotation],
	}
}
