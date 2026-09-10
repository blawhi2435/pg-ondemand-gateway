package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"
)

// Backoff bounds for retrying a temporary Accept error (e.g. a transient
// EMFILE/ENFILE), mirroring the pattern net/http's own Server.Serve uses.
const (
	acceptRetryInitialDelay = 5 * time.Millisecond
	acceptRetryMaxDelay     = 1 * time.Second
)

// Listener owns the :5432 accept loop and its graceful shutdown (design
// §9.1). Each accepted connection runs Handler.HandleConn in its own
// goroutine; Shutdown stops accepting new ones, lets existing connections
// finish naturally, and force-closes whatever's left after drainTimeout.
type Listener struct {
	ln      net.Listener
	handler *Handler

	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	draining bool

	cancel context.CancelFunc // cancels every in-flight HandleConn's context
}

// NewListener wraps ln to serve through handler.
func NewListener(ln net.Listener, handler *Handler) *Listener {
	return &Listener{
		ln:      ln,
		handler: handler,
		conns:   make(map[net.Conn]struct{}),
	}
}

// Serve accepts connections until Shutdown closes the listener. It returns
// once the listener is closed; it does not wait for in-flight connections —
// call Shutdown to do that.
//
// Serve deliberately takes no context parameter: every connection's
// lifetime is derived from context.Background() here, and the only way to
// cancel it is Shutdown, after drainTimeout elapses. A round-2 critical bug
// was exactly the alternative design — main.go passed signal.NotifyContext's
// ctx into Serve, so the instant SIGTERM arrived, every live connection's
// ctx cancelled immediately and Relay.Run tore it down right then, before
// Shutdown's "let existing connections finish" phase ever ran. Not
// accepting an external ctx here makes that mistake impossible to
// reintroduce; the signal context's only job in main.go is deciding *when*
// to call Shutdown.
func (l *Listener) Serve() error {
	// Deliberately no `defer cancel()` here: Serve returns as soon as
	// Accept errors, which happens the instant Shutdown calls l.ln.Close()
	// — well before drainTimeout. A deferred cancel would fire right then,
	// tearing down every live connection immediately regardless of the
	// timeout, exactly the bug this whole design is meant to prevent.
	// Shutdown alone decides when to call the stored l.cancel.
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	l.cancel = cancel
	l.mu.Unlock()

	var retryDelay time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			// A transient failure (classically EMFILE/ENFILE hitting the
			// nofile limit) must not permanently end acceptance while the
			// process keeps running and /readyz keeps reporting 200 — that
			// turns the pod into a silent black hole nothing restarts or
			// evicts. Retry with the same capped exponential backoff
			// net/http's own accept loop uses.
			if isTemporaryAcceptError(err) {
				retryDelay = nextAcceptRetryDelay(retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			return err
		}
		retryDelay = 0

		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.SetNoDelay(true)
		}
		if !l.track(conn) {
			continue // draining: refused and already closed
		}

		go func() {
			defer l.wg.Done()
			defer l.untrack(conn)
			l.handler.HandleConn(ctx, conn)
		}()
	}
}

// track registers conn as active and counts it in wg, atomically with
// respect to draining — both under the same critical section — and
// returns true, unless the listener is already draining (a connection can
// slip in between Shutdown closing the listener and Accept returning its
// error, so this refuses and closes it rather than let it dodge drain
// accounting).
//
// wg.Add must happen here, under l.mu, rather than as a separate statement
// in Serve after track returns: sync.WaitGroup requires that any Add with
// a positive delta starting from a zero counter happen-before the
// corresponding Wait. Doing them as two unsynchronized steps left a window
// where a connection was published into l.conns (so ActiveCount and the
// draining check already see it) but not yet counted in wg — a Shutdown
// landing in that window could call wg.Wait() against a still-zero
// counter, take the "everyone already finished" fast path, and return
// while that connection was still being handled. Guarding both under l.mu
// — the same lock Shutdown takes to set draining=true before it ever
// starts waiting — closes the window completely: whichever of track() or
// Shutdown's draining-flip acquires the lock first fully determines the
// outcome for that connection, with no partial-progress state visible to
// the other side.
func (l *Listener) track(conn net.Conn) bool {
	l.mu.Lock()
	if l.draining {
		l.mu.Unlock()
		conn.Close()
		return false
	}
	l.conns[conn] = struct{}{}
	l.wg.Add(1)
	l.mu.Unlock()
	return true
}

func (l *Listener) untrack(conn net.Conn) {
	l.mu.Lock()
	delete(l.conns, conn)
	l.mu.Unlock()
}

// isTemporaryAcceptError reports whether err is one of the small set of
// well-known transient Accept failures worth retrying instead of ending
// the accept loop — most commonly the process briefly exhausting its
// file-descriptor budget (EMFILE/ENFILE) under a connection-count spike,
// or the kernel dropping an already-queued connection before Accept could
// hand it over (ECONNABORTED). net.Error.Temporary() used to be the
// general-purpose shorthand for this, but it's deprecated — see
// https://go.dev/issue/45729: it's a poor, ever-shrinking proxy for "retry
// this" — so this names the specific conditions explicitly instead.
func isTemporaryAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED)
}

// nextAcceptRetryDelay computes the next capped-exponential backoff delay
// for a temporary Accept error, given the previous delay (0 for the first
// retry).
func nextAcceptRetryDelay(current time.Duration) time.Duration {
	if current == 0 {
		return acceptRetryInitialDelay
	}
	current *= 2
	if current > acceptRetryMaxDelay {
		return acceptRetryMaxDelay
	}
	return current
}

// Draining reports whether Shutdown has been called, for wiring into
// /readyz and pgproxy_draining.
func (l *Listener) Draining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining
}

// ActiveCount returns the number of connections currently being served,
// mainly for tests to observe drain progress.
func (l *Listener) ActiveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// Shutdown implements design §9.1's drain sequence: stop accepting, wait
// for existing connections to finish on their own, and force-close
// whatever remains once drainTimeout elapses.
func (l *Listener) Shutdown(drainTimeout time.Duration) {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()

	l.ln.Close() // Accept in Serve returns its error and the loop exits

	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Everyone finished on their own within drainTimeout; cancel purely
		// for cleanup (releasing the context's own resources) — by this
		// point every connection is already gone, so it changes nothing.
		l.cancelCtx()
		return
	case <-time.After(drainTimeout):
	}

	l.mu.Lock()
	// Cancelling ctx lets a connection already inside Relay.Run report a
	// clean reason=shutdown. But a connection still in the pre-relay phase
	// (TLS handshake, waiting on StartupMessage, waiting on
	// AuthenticationOk) blocks on a conn deadline, not ctx — cancellation
	// alone would leave it hanging until that deadline fires on its own.
	// Closing every tracked raw socket directly guarantees the force-close
	// actually happens, in every phase, right now.
	conns := make([]net.Conn, 0, len(l.conns))
	for conn := range l.conns {
		conns = append(conns, conn)
	}
	l.mu.Unlock()

	l.cancelCtx()
	for _, conn := range conns {
		conn.Close()
	}

	<-done // handler goroutines exit promptly once their conn is closed
}

// cancelCtx calls the context.CancelFunc Serve stored, if Serve has run.
// Safe to call multiple times (context.CancelFunc is idempotent).
func (l *Listener) cancelCtx() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
