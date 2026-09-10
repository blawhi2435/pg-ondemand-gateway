package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProber_Healthz_AlwaysOKRegardlessOfInformer(t *testing.T) {
	informerReady := false // simulates API server unreachable
	p := NewProber(func() bool { return informerReady })

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.Healthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Healthz status = %d, want 200 even when informer is unhealthy", rec.Code)
	}
}

func TestProber_Readyz_503WhenCacheNotSynced(t *testing.T) {
	p := NewProber(func() bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	p.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Readyz status = %d, want 503 when cache is not synced", rec.Code)
	}
}

func TestProber_Readyz_503WhileDraining(t *testing.T) {
	p := NewProber(func() bool { return true })
	p.SetDraining(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	p.Readyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Readyz status = %d, want 503 while draining", rec.Code)
	}
}

func TestProber_Readyz_200WhenSyncedAndNotDraining(t *testing.T) {
	p := NewProber(func() bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	p.Readyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Readyz status = %d, want 200 when synced and not draining", rec.Code)
	}
}
