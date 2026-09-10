package server

import (
	"net"
	"testing"
)

// timeoutOnceConn wraps a net.Conn, returning a scripted timeout error
// (net.Error with Timeout()==true) from the first N Read calls before
// delegating to the real connection — simulating relay.sharedIdleReader's
// per-direction read deadline expiring while the *other* direction is
// still within the shared idle budget (a routine, retryable event, not a
// backend failure).
type timeoutOnceConn struct {
	net.Conn
	timeoutsLeft int
}

func (c *timeoutOnceConn) Read(p []byte) (int, error) {
	if c.timeoutsLeft > 0 {
		c.timeoutsLeft--
		return 0, temporaryErr{}
	}
	return c.Conn.Read(p)
}

// TestAuthOkWatcher_TimeoutErrorDoesNotCloseClosed is the round-3
// regression test for the finding that authOkWatcher.Read treated *every*
// non-nil error as "the backend is gone", including a read-deadline
// timeout that relay.sharedIdleReader treats as routine and retries (see
// relay.go's sharedIdleReader.Read: `if netErr.Timeout() { continue }`).
// A watcher that fires Closed on a timeout races registerIfDetected into
// releasing the reservation before the backend's AuthenticationOk — which
// arrives moments later on the retried Read — is ever seen: the connection
// then authenticates and relays data successfully while never being
// registered, audited, or counted in connections_active.
func TestAuthOkWatcher_TimeoutErrorDoesNotCloseClosed(t *testing.T) {
	client, backend := net.Pipe()
	defer client.Close()
	defer backend.Close()

	conn := &timeoutOnceConn{Conn: backend, timeoutsLeft: 1}
	w := newAuthOkWatcher(conn)

	buf := make([]byte, 64)
	n, err := w.Read(buf)
	if n != 0 || err == nil {
		t.Fatalf("Read (timeout) = (%d, %v), want (0, a timeout error)", n, err)
	}
	select {
	case <-w.Closed:
		t.Fatal("Closed fired on a retryable timeout error — a slow-but-alive backend now looks like a dead one")
	default:
	}

	go func() {
		client.Write(authenticationOkPattern)
	}()
	n, err = w.Read(buf)
	if err != nil {
		t.Fatalf("Read (real data) = (%d, %v), want a clean read", n, err)
	}
	select {
	case <-w.Detected:
	default:
		t.Fatal("Detected did not fire after AuthenticationOk was read")
	}
	select {
	case <-w.Closed:
		t.Fatal("Closed fired even though the connection never actually failed")
	default:
	}
}

// TestAuthOkWatcher_RealErrorStillClosesClosed is the paired case: a
// genuine, non-timeout error (EOF, closed connection) must still close
// Closed exactly as before — the fix narrows what counts as "the backend
// is gone", it must not stop detecting it altogether.
func TestAuthOkWatcher_RealErrorStillClosesClosed(t *testing.T) {
	client, backend := net.Pipe()
	defer client.Close()

	w := newAuthOkWatcher(backend)
	client.Close() // real, permanent closure — not a timeout

	buf := make([]byte, 64)
	if _, err := w.Read(buf); err == nil {
		t.Fatal("Read after peer close: want an error")
	}
	select {
	case <-w.Closed:
	default:
		t.Fatal("Closed did not fire after a genuine connection closure")
	}
}
