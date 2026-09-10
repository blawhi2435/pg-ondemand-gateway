package relay

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestRun_ClientCloseEndsBothSides(t *testing.T) {
	client, clientPeer := net.Pipe()
	backend, backendPeer := net.Pipe()
	r := New(0) // idle timeout disabled

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	client.Close() // simulate the client disconnecting

	res := <-done
	if res.reason != ReasonClientClose {
		t.Fatalf("reason = %q, want %q", res.reason, ReasonClientClose)
	}
	assertClosed(t, "backend", backend)
}

func TestRun_BackendCloseEndsBothSides(t *testing.T) {
	client, clientPeer := net.Pipe()
	backend, backendPeer := net.Pipe()
	r := New(0)

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	backend.Close() // simulate the backend disconnecting

	res := <-done
	if res.reason != ReasonBackendClose {
		t.Fatalf("reason = %q, want %q", res.reason, ReasonBackendClose)
	}
	assertClosed(t, "client", client)
}

func TestRun_ByteCountsAreAccurate(t *testing.T) {
	client, clientPeer := net.Pipe()
	backend, backendPeer := net.Pipe()
	r := New(0)

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	clientPayload := []byte("startup message from client")
	backendPayload := []byte("query results from backend, somewhat longer")

	writeAndDrain(t, client, backend, clientPayload)
	writeAndDrain(t, backend, client, backendPayload)

	client.Close()
	res := <-done

	if res.in != int64(len(clientPayload)) {
		t.Errorf("bytesIn = %d, want %d", res.in, len(clientPayload))
	}
	if res.out != int64(len(backendPayload)) {
		t.Errorf("bytesOut = %d, want %d", res.out, len(backendPayload))
	}
}

func TestRun_IdleTimeoutCloses(t *testing.T) {
	_, clientPeer := net.Pipe()
	_, backendPeer := net.Pipe()
	r := New(20 * time.Millisecond)

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	select {
	case res := <-done:
		if res.reason != ReasonIdleTimeout {
			t.Fatalf("reason = %q, want %q", res.reason, ReasonIdleTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after idle timeout elapsed")
	}
}

// TestRun_OneDirectionalTrafficPreventsIdleTimeout is the round-2 critical
// regression test: idle detection was per-direction, so a COPY-style
// transfer (continuous backend->client traffic, silent client) was killed
// as "idle" the moment the *client* direction alone went quiet past
// idleTimeout, even though bytes were flowing continuously the other way.
// The spec requires closing only when *neither* direction has moved bytes
// within the window.
func TestRun_OneDirectionalTrafficPreventsIdleTimeout(t *testing.T) {
	client, clientPeer := net.Pipe()
	backend, backendPeer := net.Pipe()
	const idleTimeout = 80 * time.Millisecond
	r := New(idleTimeout)

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	// The client never sends anything at all (a real client waiting on a
	// long-running query does exactly this). The backend streams
	// continuously, each write well inside idleTimeout of the last, for
	// longer than idleTimeout in total.
	stop := time.Now().Add(3 * idleTimeout)
	go func() {
		buf := make([]byte, 1)
		for time.Now().Before(stop) {
			io.ReadFull(client, buf) // drain what the relay forwards
		}
	}()
	for time.Now().Before(stop) {
		if _, err := backend.Write([]byte("x")); err != nil {
			break
		}
		time.Sleep(idleTimeout / 4)
	}

	select {
	case res := <-done:
		t.Fatalf("Run returned (reason=%q) while one-directional traffic was still flowing", res.reason)
	default:
	}

	client.Close()
	backend.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after both sides closed")
	}
}

// TestRun_BothDirectionsSilentStillTimesOut guards against the shared-idle
// fix accidentally disabling idle detection altogether: if *neither*
// direction has any traffic, the connection must still close.
func TestRun_BothDirectionsSilentStillTimesOut(t *testing.T) {
	_, clientPeer := net.Pipe()
	_, backendPeer := net.Pipe()
	r := New(30 * time.Millisecond)

	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(context.Background(), clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	select {
	case res := <-done:
		if res.reason != ReasonIdleTimeout {
			t.Fatalf("reason = %q, want %q", res.reason, ReasonIdleTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after both directions stayed idle")
	}
}

func TestRun_ContextCancelEndsWithShutdownReason(t *testing.T) {
	_, clientPeer := net.Pipe()
	_, backendPeer := net.Pipe()
	r := New(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() {
		in, out, reason := r.Run(ctx, clientPeer, backendPeer)
		done <- result{in, out, reason}
	}()

	cancel()

	select {
	case res := <-done:
		if res.reason != ReasonShutdown {
			t.Fatalf("reason = %q, want %q", res.reason, ReasonShutdown)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

type result struct {
	in, out int64
	reason  string
}

// writeAndDrain writes payload on w and reads it back to completion on r,
// synchronizing net.Pipe's unbuffered, synchronous semantics.
func writeAndDrain(t *testing.T, w, r net.Conn, payload []byte) {
	t.Helper()
	writeDone := make(chan error, 1)
	go func() {
		_, err := w.Write(payload)
		writeDone <- err
	}()

	buf := make([]byte, len(payload))
	if _, err := readFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFull(r net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// assertClosed checks that conn's peer has been closed by trying a read,
// which should fail promptly once the relay closes its end.
func assertClosed(t *testing.T, label string, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("%s side: expected read to fail after relay closed it", label)
	}
}
