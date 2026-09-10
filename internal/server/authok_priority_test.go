package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/route"
)

// TestAwaitAuthOutcome_DetectedWinsWhenBothReady is the round-3 regression
// test for the select-randomness bug: authOkWatcher.Read scans for
// AuthenticationOk *and* closes Closed on any read error, so a single Read
// that returns both the AuthenticationOk bytes and a terminal error (a
// backend that authenticates and then closes promptly — health checks,
// `psql -c`, any short session) closes both channels. The original
// three-way select had no priority between them, so Go picked uniformly at
// random: about half of authenticated short connections were never
// registered, producing a disconnect audit event with no matching connect
// event.
//
// Both channels are pre-closed here (rather than raced via real I/O
// timing) specifically so this test is deterministic: with the bug, roughly
// half of these trials would take the Closed branch and return false.
func TestAwaitAuthOutcome_DetectedWinsWhenBothReady(t *testing.T) {
	const trials = 500

	for i := 0; i < trials; i++ {
		client, _ := net.Pipe()
		backend, _ := net.Pipe()
		watched := newAuthOkWatcher(backend)
		close(watched.Detected)
		close(watched.Closed) // simulates the same Read closing both

		reg := registry.NewInMemoryRegistry(fixedLimits(0, 0))
		if !reg.TryAdmit("tenant1") {
			t.Fatal("TryAdmit: unexpectedly rejected")
		}
		h := &Handler{
			Registry: reg,
			Metrics:  metrics.New(),
			Audit:    audit.NewSink(io.Discard),
		}

		ok := h.awaitAuthOutcome(context.Background(), watched, "c1",
			route.Route{Cluster: "tenant1"}, "tenant1.db.test", "10.0.0.1",
			pgwire.StartupMessage{User: "app_rw", Database: "orders"},
			client, backend, time.Now())
		if !ok {
			t.Fatalf("trial %d: awaitAuthOutcome did not register despite AuthenticationOk having been observed", i)
		}
	}
}

// TestAwaitAuthOutcome_ReleasesWhenNeverDetected confirms the other half
// still works: a connection that closes without ever authenticating must
// still release its reservation and report false.
func TestAwaitAuthOutcome_ReleasesWhenNeverDetected(t *testing.T) {
	client, _ := net.Pipe()
	backend, _ := net.Pipe()
	watched := newAuthOkWatcher(backend)
	close(watched.Closed) // Detected never fires

	reg := registry.NewInMemoryRegistry(fixedLimits(0, 1))
	if !reg.TryAdmit("tenant1") {
		t.Fatal("TryAdmit: unexpectedly rejected")
	}
	h := &Handler{Registry: reg, Metrics: metrics.New(), Audit: audit.NewSink(io.Discard)}

	ok := h.awaitAuthOutcome(context.Background(), watched, "c1",
		route.Route{Cluster: "tenant1"}, "tenant1.db.test", "10.0.0.1",
		pgwire.StartupMessage{User: "app_rw", Database: "orders"},
		client, backend, time.Now())
	if ok {
		t.Fatal("awaitAuthOutcome registered a connection that never authenticated")
	}
	if !reg.TryAdmit("tenant1") {
		t.Fatal("reservation was not released: a fresh TryAdmit for the same cluster was rejected")
	}
}
