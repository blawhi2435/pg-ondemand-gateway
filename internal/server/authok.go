package server

import (
	"bytes"
	"errors"
	"net"
	"sync"
)

// authenticationOkPattern is the wire-format bytes of an unadorned
// AuthenticationOk message: type 'R', length 8 (self-inclusive), auth type
// 0. Design step 8 requires registering the connection only once this has
// been read from the backend — before that, the client-claimed user is
// unverified.
var authenticationOkPattern = []byte{'R', 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00}

// authOkWatcher wraps a backend net.Conn and closes Detected exactly once,
// the moment AuthenticationOk is observed crossing a Read call. It changes
// nothing about the bytes it returns — relay.Relay reads through it exactly
// as it would the raw connection — so it can wrap the backend conn passed
// into relay.Run without altering the data-relay behavior at all.
//
// It also closes Closed exactly once if a Read ever fails before
// AuthenticationOk was seen — the realistic backend-auth-failure case
// (wrong password, pg_hba rejection: an ErrorResponse followed by the
// backend closing). Without this, the goroutine waiting on Detected would
// park forever, since nothing else ever wakes it.
type authOkWatcher struct {
	net.Conn
	Detected chan struct{}
	Closed   chan struct{}

	detectedOnce sync.Once
	closedOnce   sync.Once
	pending      []byte // tail carried across Read calls, bounded to pattern length
}

// newAuthOkWatcher wraps conn for AuthenticationOk detection.
func newAuthOkWatcher(conn net.Conn) *authOkWatcher {
	return &authOkWatcher{Conn: conn, Detected: make(chan struct{}), Closed: make(chan struct{})}
}

// Read implements net.Conn, scanning each chunk (plus a short carried-over
// tail, in case the pattern straddles two reads) for authenticationOkPattern.
func (w *authOkWatcher) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		w.scan(p[:n])
	}
	if err != nil && !isTimeout(err) {
		// A timeout is not "the backend is gone": relay.sharedIdleReader
		// sets a short per-direction read deadline and explicitly retries
		// on net.Error.Timeout() (see relay.go), re-arming it as long as
		// the shared idle budget hasn't elapsed. Closing Closed here for a
		// timeout would tell registerIfDetected to give up and release the
		// reservation while the backend is still about to send
		// AuthenticationOk on the very next retried Read — producing a
		// connection that authenticates and relays successfully while
		// never being registered, audited, or counted as active.
		w.closedOnce.Do(func() { close(w.Closed) })
	}
	return n, err
}

// isTimeout reports whether err is a net.Error whose Timeout() is true.
// Timeout() (unlike the deprecated Temporary()) is still the documented way
// to distinguish "this specific read/write deadline elapsed" from a
// permanent failure.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (w *authOkWatcher) scan(chunk []byte) {
	w.pending = append(w.pending, chunk...)
	if bytes.Contains(w.pending, authenticationOkPattern) {
		w.detectedOnce.Do(func() { close(w.Detected) })
	}
	// Keep only enough tail to catch a pattern split across the next Read.
	if keep := len(authenticationOkPattern) - 1; len(w.pending) > keep {
		w.pending = w.pending[len(w.pending)-keep:]
	}
}
