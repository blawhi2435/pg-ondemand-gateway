package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
)

func gssEncRequestPacket() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], 80877104)
	return buf
}

// TestHandleConn_GSSENCRequest_RepliesNAndFallsBackToSSLRequest covers
// pgwire-protocol Scenario「GSSENCRequest」: reply 'N', then — this is the
// easy-to-break part, since every other case in HandleConn's switch
// returns — the loop must keep going and accept a following SSLRequest on
// the same connection.
func TestHandleConn_GSSENCRequest_RepliesNAndFallsBackToSSLRequest(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	h := newTestHandler(t, clientCert, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(gssEncRequestPacket())
	reply := make([]byte, 1)
	if _, err := io.ReadFull(clientSide, reply); err != nil {
		t.Fatalf("read GSSENCRequest reply: %v", err)
	}
	if reply[0] != 'N' {
		t.Fatalf("GSSENCRequest reply = %q, want 'N'", reply[0])
	}

	// The connection must still be open and accept a following SSLRequest.
	clientSide.Write(sslRequestPacket())
	if _, err := io.ReadFull(clientSide, reply); err != nil {
		t.Fatalf("read SSLRequest reply after GSSENCRequest: %v", err)
	}
	if reply[0] != 'S' {
		t.Fatalf("SSLRequest reply after GSSENCRequest = %q, want 'S'", reply[0])
	}

	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake after GSSENCRequest fallback: %v", err)
	}
	// The handshake deadline is only cleared once the connection reaches
	// the relay phase (after a full StartupMessage/auth exchange), which
	// this test doesn't drive it to — close explicitly rather than wait for
	// that deadline to end the connection.
	clientSide.Close()
	<-done
}

// TestHandleConn_UnknownFirstPacket_BadStartup covers pgwire-protocol
// Scenario「未知的首包」: an unrecognized code closes the connection and
// counts handshake_errors_total{reason="bad_startup"}.
func TestHandleConn_UnknownFirstPacket_BadStartup(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()
	h := newTestHandler(t, clientCert, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	garbage := make([]byte, 8)
	binary.BigEndian.PutUint32(garbage[0:4], 8)
	binary.BigEndian.PutUint32(garbage[4:8], 999999999)
	go clientSide.Write(garbage)

	buf := make([]byte, 1)
	clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := clientSide.Read(buf); err == nil {
		t.Fatal("expected the connection to be closed with no reply for an unknown first packet")
	}
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "bad_startup"}, 1) {
		t.Error("handshake_errors_total{reason=bad_startup} was not incremented")
	}
}

// TestHandleConn_TLSHandshakeFailure_CountsTLSError covers the
// upgradeToTLS failure branch: a client that can't complete the TLS
// handshake (wrong CA) must be counted as tls_error and the socket closed.
func TestHandleConn_TLSHandshakeFailure_CountsTLSError(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")
	_, wrongCADER := genTestCert(t, "unrelated")
	clientSide, proxyClientSide := net.Pipe()
	h := newTestHandler(t, clientCert, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)

	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, wrongCADER), // does not trust the server's actual cert
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err == nil {
		t.Fatal("client TLS handshake unexpectedly succeeded against the wrong CA")
	}
	clientSide.Close()

	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "tls_error"}, 1) {
		t.Error("handshake_errors_total{reason=tls_error} was not incremented")
	}
}

// TestHandleConn_GlobalMaxConns covers connection-lifecycle Scenario
// 「超過全域上限 → 不論屬於哪個 cluster 皆回覆 53300」— distinct from the
// per-cluster case already covered elsewhere.
func TestHandleConn_GlobalMaxConns(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.Registry = registry.NewInMemoryRegistry(func() registry.Limits { return registry.Limits{MaxConns: 1} })
	})
	// Fill the global limit with a connection in a *different* cluster.
	h.Registry.Add(registry.NewConn(registry.ConnParams{
		ConnID: "existing", Cluster: "tenant2", SNI: "tenant2.db.test",
		ClientIP: "10.0.0.1", User: "u", Database: "d", Backend: "b", StartedAt: time.Now(),
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, tlsConn)
	if !bytes.Contains(errResp, []byte(pgwire.SQLStateTooManyConnections)) {
		t.Errorf("ErrorResponse missing SQLSTATE %s: %x", pgwire.SQLStateTooManyConnections, errResp)
	}
	drainAndClose(tlsConn)
	<-done

	total, _ := h.Registry.Count()
	if total != 1 {
		t.Errorf("registry total = %d, want 1 (the rejected connection must not be added)", total)
	}
}

// TestFailBackend_ClosesBothSidesAndCountsBackendError exercises
// failBackend directly: it has 0% coverage otherwise, and it's exactly the
// "error path that closes sockets, where a leak would be invisible"
// category.
func TestFailBackend_ClosesBothSidesAndCountsBackendError(t *testing.T) {
	client, clientPeer := net.Pipe()
	backend, backendPeer := net.Pipe()

	h := &Handler{Metrics: metrics.New()}
	failBackendDone := make(chan struct{})
	go func() {
		defer close(failBackendDone)
		h.failBackend(clientPeer, backendPeer, "tenant1")
	}()

	buf := make([]byte, 1)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("client did not receive an ErrorResponse: %v", err)
	}
	if buf[0] != 'E' {
		t.Fatalf("first byte = %q, want 'E' (ErrorResponse)", buf[0])
	}
	// rejectAndClose's Write blocks (net.Pipe is synchronous) until every
	// byte of the ErrorResponse is consumed, not just this first one.
	go io.Copy(io.Discard, client)

	backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := backend.Read(buf); err == nil {
		t.Fatal("backend side was not closed")
	}

	select {
	case <-failBackendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("failBackend did not return")
	}

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "backend_error"}, 1) {
		t.Error("connections_total{result=backend_error} was not incremented")
	}
}

// TestDialBackend_HandshakeFailure_Returns08006 covers pgwire-protocol's
// SQLSTATE mapping for「後端握手失敗」specifically (as opposed to「後端連不
// 上」, which handler_reject_test.go already covers): the backend answers
// SSLRequest but presents a certificate signed by a CA the proxy doesn't
// trust.
func TestDialBackend_HandshakeFailure_Returns08006(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	untrustedBackendCert, _ := genTestCert(t, "backend.internal")
	_, trustedBackendCADER := genTestCert(t, "some-other-ca")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.BackendTLS = &tls.Config{RootCAs: certPool(t, trustedBackendCADER)} // does not trust untrustedBackendCert
		h.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return proxyBackendSide, nil
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	go func() {
		header := make([]byte, 8)
		io.ReadFull(backendSide, header)
		backendSide.Write([]byte{'S'})
		tlsConn := tls.Server(backendSide, &tls.Config{Certificates: []tls.Certificate{untrustedBackendCert}})
		tlsConn.Handshake() // the proxy will reject this; ignore the resulting error here
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, tlsConn)
	if !bytes.Contains(errResp, []byte(pgwire.SQLStateConnectionFailure)) {
		t.Errorf("ErrorResponse missing SQLSTATE %s: %x", pgwire.SQLStateConnectionFailure, errResp)
	}
	drainAndClose(tlsConn)
	<-done
}

// --- handleCancel negative paths (26.2) ---

func TestHandleCancel_TruncatedBody_BadStartup(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()
	h := newTestHandler(t, clientCert, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()

	// A CancelRequest header (length=16) but the connection closes before
	// sending the 8-byte pid+secret body.
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], 16)
	binary.BigEndian.PutUint32(header[4:8], 80877102)
	tlsConn.Write(header)
	tlsConn.Close()

	<-done
	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "bad_startup"}, 1) {
		t.Error("handshake_errors_total{reason=bad_startup} was not incremented for a truncated CancelRequest body")
	}
}

func TestHandleCancel_NoTLS_UnknownSNI(t *testing.T) {
	h := &Handler{Metrics: metrics.New(), Timeouts: Timeouts{Handshake: 2 * time.Second}}
	clientSide, proxyClientSide := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(cancelRequestPacket(1, 2))
	drainAndClose(clientSide)
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "unknown_sni"}, 1) {
		t.Error("handshake_errors_total{reason=unknown_sni} was not incremented for a plaintext CancelRequest (no SNI available)")
	}
}

func TestHandleCancel_UnknownSNI(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()
	h := newTestHandler(t, clientCert, nil) // empty router: SNI unknown

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()
	tlsConn.Write(cancelRequestPacket(1, 2))
	drainAndClose(tlsConn)
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "unknown_sni"}, 1) {
		t.Error("handshake_errors_total{reason=unknown_sni} was not incremented")
	}
}

func TestHandleCancel_BackendDialFailure_CountsBackendError(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()
	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()
	tlsConn.Write(cancelRequestPacket(1, 2))
	drainAndClose(tlsConn)
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "backend_error"}, 1) {
		t.Error("connections_total{cluster=tenant1,result=backend_error} was not incremented for a cancel that failed to dial")
	}
}

// --- registerConnection's Add-failure branch (26.2) ---

// failingAddRegistry always fails Add, regardless of TryAdmit, so
// registerConnection's defensive close-both-sockets branch can be exercised
// directly.
type failingAddRegistry struct {
	*registry.InMemoryRegistry
}

func (r *failingAddRegistry) Add(*registry.Conn) error {
	return registry.ErrTooManyConns
}

func TestRegisterConnection_AddFailure_ClosesBothSocketsNoConnectEvent(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	var auditBuf bytes.Buffer
	reg := &failingAddRegistry{InMemoryRegistry: registry.NewInMemoryRegistry(fixedLimits(0, 0))}
	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.Registry = reg
		h.Audit = audit.NewSink(&auditBuf)
		h.BackendTLS = &tls.Config{RootCAs: certPool(t, backendCADER)}
		h.Dial = func(ctx context.Context, addr string) (net.Conn, error) { return proxyBackendSide, nil }
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	go func() {
		header := make([]byte, 8)
		io.ReadFull(backendSide, header)
		backendSide.Write([]byte{'S'})
		tlsConn := tls.Server(backendSide, &tls.Config{Certificates: []tls.Certificate{backendCert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		startupHeader, err := pgwire.ReadHeader(tlsConn)
		if err != nil {
			return
		}
		payload := make([]byte, int(startupHeader.Length)-pgwire.HeaderSize)
		io.ReadFull(tlsConn, payload)
		tlsConn.Write([]byte(authOkBytes))
		drainAndClose(tlsConn)
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.Handshake()
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	// Relay.Run and the registration goroutine run concurrently, so the
	// client may still see whatever bytes (e.g. AuthenticationOk) crossed
	// before Add's failure was noticed — that race is exactly why a clean
	// 53300 can't be sent at this point (see registerConnection's comment).
	// What must hold regardless is that the connection does not stay open
	// and usable: draining it must reach a close (io.Copy returns nil on a
	// clean EOF, which is exactly the success case here) well before the
	// deadline — if it instead blocks until the deadline fires, that's the
	// bug (both sides never actually got closed).
	tlsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	io.Copy(io.Discard, tlsConn)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("draining the connection took %v — it looks like the deadline fired rather than the connection actually closing", elapsed)
	}
	<-done

	if bytesContainsConnect(auditBuf.Bytes()) {
		t.Errorf("a connect event must not be emitted when Add fails:\n%s", auditBuf.String())
	}
	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "limit_exceeded"}, 1) {
		t.Error("connections_total{result=limit_exceeded} was not incremented")
	}
}

func bytesContainsConnect(b []byte) bool {
	return bytes.Contains(b, []byte(`"event":"connect"`))
}
