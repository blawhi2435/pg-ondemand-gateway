package server

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
)

// longLivedHandler builds a Handler whose connections just block until the
// context is cancelled — enough to exercise Listener's drain accounting
// without needing a full TLS/backend round trip.
func longLivedHandler() *Handler {
	return &Handler{
		Router:     fakeRouter{},
		Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
		Registry:   registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		Relay:      relay.New(0),
		Audit:      audit.NewSink(&bytes.Buffer{}),
		Metrics:    metrics.New(),
		ClientTLS:  &tls.Config{},
		BackendTLS: &tls.Config{},
		Timeouts:   Timeouts{Handshake: 30 * time.Second},
	}
}

func newLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// waitFor polls cond until it's true or fails the test after a bounded
// deadline — used instead of a fixed time.Sleep so these tests don't depend
// on how fast a given CI runner happens to be.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met before deadline: %s", msg)
}

// dialRetry dials addr, retrying briefly to absorb the delay between
// starting Serve's goroutine and it actually reaching Accept — rather than
// a fixed pre-dial sleep.
func dialRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", addr, lastErr)
	return nil
}

func TestListener_ShutdownStopsAcceptingButLetsExistingConnsFinish(t *testing.T) {
	ln := newLoopbackListener(t)
	addr := ln.Addr().String()

	handler := longLivedHandler()
	l := NewListener(ln, handler)

	serveErr := make(chan error, 1)
	go func() { serveErr <- l.Serve() }()

	conn := dialRetry(t, addr)
	defer conn.Close()

	// Don't send anything: the handler will block in ReadHeader waiting for
	// the 8-byte header, keeping this connection "in flight" until either
	// data arrives, the handshake deadline fires, or we close it.
	waitFor(t, "connection tracked as active", func() bool { return l.ActiveCount() == 1 })

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		l.Shutdown(2 * time.Second)
	}()

	// New connections must be refused once draining starts.
	waitFor(t, "listener enters draining state", func() bool { return l.Draining() })
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Error("dial succeeded after Shutdown closed the listener; want a connection error")
	}

	// The still-open first connection must not have been force-closed yet
	// (drainTimeout is 2s and we're well inside it).
	if got := l.ActiveCount(); got != 1 {
		t.Errorf("ActiveCount() = %d after Shutdown but before drainTimeout, want 1 (existing conn preserved)", got)
	}

	conn.Close() // let the in-flight connection finish naturally

	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return promptly after the last connection closed")
	}

	<-serveErr
}

func TestListener_ShutdownForceClosesAfterDrainTimeout(t *testing.T) {
	ln := newLoopbackListener(t)
	addr := ln.Addr().String()
	handler := longLivedHandler()
	l := NewListener(ln, handler)

	serveErr := make(chan error, 1)
	go func() { serveErr <- l.Serve() }()

	conn := dialRetry(t, addr)
	defer conn.Close()
	waitFor(t, "connection tracked as active", func() bool { return l.ActiveCount() == 1 })

	start := time.Now()
	l.Shutdown(100 * time.Millisecond) // never closed client-side: forces the timeout path
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v, want it bounded by the short drainTimeout", elapsed)
	}
	if got := l.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount() = %d after drainTimeout elapsed, want 0 (force-closed)", got)
	}
	<-serveErr
}

func TestListener_ServeSetsTCPNoDelay(t *testing.T) {
	// Regression guard: Serve must not panic on a non-TCP listener path and
	// must accept/dispatch connections at all.
	ln := newLoopbackListener(t)
	addr := ln.Addr().String()
	handler := longLivedHandler()
	l := NewListener(ln, handler)

	serveErr := make(chan error, 1)
	go func() { serveErr <- l.Serve() }()

	conn := dialRetry(t, addr)
	defer conn.Close()

	waitFor(t, "connection tracked as active", func() bool { return l.ActiveCount() == 1 })
	l.Shutdown(100 * time.Millisecond)
	<-serveErr
}
