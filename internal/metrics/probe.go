package metrics

import (
	"net/http"
	"sync/atomic"
)

// Prober serves /healthz and /readyz (design §9.4b).
//
// /healthz never checks informer state: an informer wedged on an
// unreachable API server can't be fixed by restarting the pod (a restart
// needs that same API server), so tying liveness to it would only add a
// second failure mode on top of the first.
type Prober struct {
	informerReady func() bool
	draining      atomic.Bool
}

// NewProber returns a Prober whose readiness depends on informerReady
// (typically route.InformerRouter.Ready) and on drain state.
func NewProber(informerReady func() bool) *Prober {
	return &Prober{informerReady: informerReady}
}

// SetDraining marks the pod as draining (or not), affecting Readyz only.
func (p *Prober) SetDraining(draining bool) {
	p.draining.Store(draining)
}

// Healthz reports whether the process is alive. Always 200.
func (p *Prober) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// Readyz reports whether this pod should currently receive new connections:
// 503 until the route table's initial cache sync completes, and 503 again
// once draining starts.
func (p *Prober) Readyz(w http.ResponseWriter, _ *http.Request) {
	if !p.informerReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if p.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
