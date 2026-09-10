// Package relay implements seam D: bidirectional byte forwarding between a
// client and backend connection (design §5, §9.4). Phase 1's implementation
// is a plain io.Copy in each direction; keeping it behind the Relay
// interface lets phase 2 swap in a message-parsing implementation without
// touching connection management, timeouts, or close ordering.
package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/reason"
)

// Disconnect reasons produced by Run, aliasing the single shared source in
// internal/reason so relay and audit can never drift apart on these
// values. Other reasons in the log format's value domain (backend_error,
// limit_exceeded) are set by the caller for events that happen outside Run
// itself.
const (
	ReasonClientClose  = reason.ClientClose
	ReasonBackendClose = reason.BackendClose
	ReasonIdleTimeout  = reason.IdleTimeout
	ReasonShutdown     = reason.Shutdown
)

// bufferSize matches io.Copy's own default internal buffer size.
const bufferSize = 32 * 1024

// Relay is seam D.
type Relay interface {
	// Run copies bytes between client and backend until either side closes,
	// errors, goes idle past the configured timeout, or ctx is cancelled.
	// Both sides are closed before Run returns. bytesIn is bytes read from
	// client (written to backend); bytesOut is bytes read from backend
	// (written to client).
	Run(ctx context.Context, client, backend net.Conn) (bytesIn, bytesOut int64, reason string)
}

// IOCopyRelay is the phase 1 Relay implementation.
type IOCopyRelay struct {
	idleTimeout time.Duration
	bufPool     *sync.Pool
}

// New returns a Relay that closes the connection once *neither* direction
// has moved any bytes for idleTimeout. idleTimeout <= 0 disables idle
// detection.
func New(idleTimeout time.Duration) *IOCopyRelay {
	return &IOCopyRelay{
		idleTimeout: idleTimeout,
		bufPool: &sync.Pool{
			New: func() any { return make([]byte, bufferSize) },
		},
	}
}

// Run implements Relay.
func (r *IOCopyRelay) Run(ctx context.Context, client, backend net.Conn) (bytesIn, bytesOut int64, reason string) {
	var (
		once        sync.Once
		finalReason string
	)
	closeOnce := func(why string) {
		once.Do(func() {
			finalReason = why
			client.Close()
			backend.Close()
		})
	}

	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			closeOnce(ReasonShutdown)
		case <-stopWatcher:
		}
	}()

	// Shared across both directions: idle timeout only fires once *neither*
	// direction has moved a byte within the window (design §9.4 — "雙向皆無
	// 流量"). A per-direction deadline would kill a COPY-style one-way
	// transfer the moment the silent direction alone crossed the timeout.
	var tracker *idleTracker
	if r.idleTimeout > 0 {
		tracker = newIdleTracker()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, timedOut := r.copyDirection(backend, client, tracker)
		bytesIn = n
		closeOnce(pickReason(timedOut, ReasonClientClose))
	}()
	go func() {
		defer wg.Done()
		n, timedOut := r.copyDirection(client, backend, tracker)
		bytesOut = n
		closeOnce(pickReason(timedOut, ReasonBackendClose))
	}()
	wg.Wait()

	return bytesIn, bytesOut, finalReason
}

// copyDirection copies from src to dst using a pooled buffer, applying a
// shared idle deadline on src when tracker is non-nil. timedOut reports
// whether the copy ended because the shared idle window elapsed, as opposed
// to a clean EOF or a forced close from the other direction finishing first.
func (r *IOCopyRelay) copyDirection(dst io.Writer, src net.Conn, tracker *idleTracker) (n int64, timedOut bool) {
	buf := r.bufPool.Get().([]byte)
	defer r.bufPool.Put(buf)

	var reader io.Reader = src
	if tracker != nil {
		reader = &sharedIdleReader{conn: src, timeout: r.idleTimeout, tracker: tracker}
	}

	n, err := io.CopyBuffer(dst, reader, buf)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		timedOut = true
	}
	return n, timedOut
}

// pickReason returns ReasonIdleTimeout when timedOut, otherwise fallback.
func pickReason(timedOut bool, fallback string) string {
	if timedOut {
		return ReasonIdleTimeout
	}
	return fallback
}

// idleTracker records the last time either relay direction moved a byte.
// Shared between both copyDirection goroutines so idle detection reflects
// the connection as a whole rather than one direction in isolation.
type idleTracker struct {
	lastActivity atomic.Int64 // UnixNano
}

func newIdleTracker() *idleTracker {
	t := &idleTracker{}
	t.touch()
	return t
}

func (t *idleTracker) touch() {
	t.lastActivity.Store(time.Now().UnixNano())
}

func (t *idleTracker) idleFor() time.Duration {
	return time.Since(time.Unix(0, t.lastActivity.Load()))
}

// errIdleTimeout is returned by sharedIdleReader once the whole connection
// (both directions) has been silent for the configured window. It
// implements net.Error so copyDirection's existing timeout classification
// picks it up without special-casing.
type errIdleTimeout struct{}

func (errIdleTimeout) Error() string   { return "relay: idle timeout" }
func (errIdleTimeout) Timeout() bool   { return true }
func (errIdleTimeout) Temporary() bool { return false }

// sharedIdleReader resets src's read deadline before every Read, but
// instead of failing as soon as *this* direction goes quiet, it consults
// the shared tracker: if the other direction has had recent activity, it
// re-arms and keeps waiting. Only once the tracker itself reports
// idleFor() >= timeout — meaning neither direction has moved a byte — does
// Read return an idle-timeout error.
type sharedIdleReader struct {
	conn    net.Conn
	timeout time.Duration
	tracker *idleTracker
}

func (d *sharedIdleReader) Read(p []byte) (int, error) {
	for {
		remaining := d.timeout - d.tracker.idleFor()
		if remaining <= 0 {
			return 0, errIdleTimeout{}
		}
		if err := d.conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return 0, err
		}

		n, err := d.conn.Read(p)
		if n > 0 {
			d.tracker.touch()
			return n, nil
		}
		if err == nil {
			continue // spurious zero-byte, no-error read; re-arm and retry
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			continue // this direction alone timed out; re-check the shared tracker
		}
		return 0, err // a real error: EOF, closed connection, etc.
	}
}
