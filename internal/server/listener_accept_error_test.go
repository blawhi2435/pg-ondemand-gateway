package server

import (
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeAcceptListener implements net.Listener with a scripted sequence of
// Accept behaviors, for testing Listener.Serve's error handling without a
// real socket. Once the script is exhausted, Accept blocks until Close is
// called — mirroring how a real net.Listener's Close unblocks a pending
// Accept — so tests can cleanly shut down instead of leaking the Serve
// goroutine (and whatever it dispatched) past the end of the test.
type fakeAcceptListener struct {
	acceptN atomic.Int32
	script  []func() (net.Conn, error)
	closed  atomic.Bool
	closeCh chan struct{}
}

func newFakeAcceptListener(script ...func() (net.Conn, error)) *fakeAcceptListener {
	return &fakeAcceptListener{script: script, closeCh: make(chan struct{})}
}

func (f *fakeAcceptListener) Accept() (net.Conn, error) {
	i := int(f.acceptN.Add(1)) - 1
	if i >= len(f.script) {
		<-f.closeCh
		return nil, net.ErrClosed
	}
	return f.script[i]()
}

func (f *fakeAcceptListener) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		close(f.closeCh)
	}
	return nil
}

func (f *fakeAcceptListener) Addr() net.Addr { return &net.TCPAddr{} }

// temporaryErr implements net.Error with Timeout()/Temporary() both true,
// simulating a transient accept failure (e.g. EMFILE).
type temporaryErr struct{}

func (temporaryErr) Error() string   { return "temporary: too many open files" }
func (temporaryErr) Timeout() bool   { return true }
func (temporaryErr) Temporary() bool { return true }

// TestListener_Serve_RetriesOnTemporaryAcceptError is the round-2
// regression test for the finding that any Accept error — including a
// transient EMFILE/ENFILE — permanently ended the accept loop while the
// process kept running and /readyz kept reporting 200 (since it only
// consults informer/drain state), leaving the pod a silent black hole.
func TestListener_Serve_RetriesOnTemporaryAcceptError(t *testing.T) {
	fl := newFakeAcceptListener(
		func() (net.Conn, error) { return nil, syscall.EMFILE },
		func() (net.Conn, error) { return nil, syscall.ENFILE },
		func() (net.Conn, error) {
			client, _ := net.Pipe()
			return client, nil
		},
	)
	l := NewListener(fl, longLivedHandler())

	serveDone := make(chan error, 1)
	go func() { serveDone <- l.Serve() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fl.acceptN.Load() >= 3 {
			break // Serve kept calling Accept past the two temporary errors
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fl.acceptN.Load() < 3 {
		t.Fatal("Serve gave up after a temporary Accept error instead of retrying")
	}

	// Clean up: the script's third call handed Serve a real connection,
	// which longLivedHandler now holds open — Shutdown's force-close path
	// (short drainTimeout, since nothing will ever close it on its own)
	// closes it and lets Serve's now-blocked-on-closeCh Accept return too.
	l.Shutdown(200 * time.Millisecond)
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

// permanentErr is a plain error: not net.ErrClosed, not a net.Error at all
// — an unexpected, non-recoverable Accept failure.
var errPermanentAcceptFailure = errors.New("simulated permanent accept failure")

// TestListener_Serve_ReturnsOnPermanentAcceptError is the other half of
// 25.9/25.10: an error that is neither "temporary" nor the expected
// close-during-shutdown sentinel must end the loop and be reported to the
// caller, so main.go can flip readiness rather than silently running with
// a dead accept loop.
func TestListener_Serve_ReturnsOnPermanentAcceptError(t *testing.T) {
	fl := newFakeAcceptListener(
		func() (net.Conn, error) { return nil, errPermanentAcceptFailure },
	)
	l := NewListener(fl, longLivedHandler())

	serveDone := make(chan error, 1)
	go func() { serveDone <- l.Serve() }()

	select {
	case err := <-serveDone:
		if !errors.Is(err, errPermanentAcceptFailure) {
			t.Fatalf("Serve returned %v, want it to wrap errPermanentAcceptFailure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after a permanent Accept error")
	}
}
