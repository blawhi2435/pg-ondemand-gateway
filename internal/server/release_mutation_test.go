package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
)

// assertNoCapacityLeak shares one InMemoryRegistry (per-cluster cap of 1)
// across `iterations` sequential connections that are each expected to
// fail via the given dialer/backend script, then asserts TryAdmit still
// succeeds afterward — proving every one of those failures released its
// reservation. This is deliberately the same proof shape the test review
// used to demonstrate the leak: delete the Release call this test targets,
// and it must go red.
func assertNoCapacityLeak(t *testing.T, iterations int, buildHandler func(reg *registry.InMemoryRegistry) *Handler, driveOneConnection func(h *Handler)) {
	t.Helper()
	reg := registry.NewInMemoryRegistry(func() registry.Limits { return registry.Limits{MaxConnsPerCluster: 1} })

	for i := 0; i < iterations; i++ {
		h := buildHandler(reg)
		driveOneConnection(h)

		total, _ := reg.Count()
		if total != 0 {
			t.Fatalf("iteration %d: registry has %d committed connections, want 0 (all of these fail before Add)", i, total)
		}
		if !reg.TryAdmit("tenant1") {
			t.Fatalf("capacity leak after iteration %d: TryAdmit refused even though every prior connection failed before ever registering — a Release call was skipped", i)
		}
		reg.Release("tenant1") // undo the probe admission so the next iteration starts clean
	}
}

// TestRelease_DialFailure is the mutation-provable test for
// handler.go's Release call after a backend dial failure.
func TestRelease_DialFailure(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")

	assertNoCapacityLeak(t, 3,
		func(reg *registry.InMemoryRegistry) *Handler {
			return &Handler{
				Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
				Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
				Registry:   reg,
				Relay:      relay.New(0),
				Audit:      audit.NewSink(io.Discard),
				Metrics:    metrics.New(),
				ClientTLS: &tls.Config{
					Certificates: []tls.Certificate{clientCert},
					MinVersion:   tls.VersionTLS12,
				},
				BackendTLS: &tls.Config{},
				Dial: func(ctx context.Context, addr string) (net.Conn, error) {
					return nil, errors.New("connection refused")
				},
				Timeouts: Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
			}
		},
		func(h *Handler) {
			clientSide, proxyClientSide := net.Pipe()
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
			readErrorResponse(t, tlsConn)
			drainAndClose(tlsConn)
			<-done
		})
}

// runBackendThatFailsAfterNBytes plays the backend side of a connection:
// completes the standard SSLRequest+TLS handshake, reads exactly
// readBeforeClose bytes of whatever the proxy forwards, then closes —
// making the proxy's *next* write to the backend fail.
func runBackendThatFailsAfterNBytes(t *testing.T, conn net.Conn, backendCert tls.Certificate, readBeforeClose int) {
	t.Helper()
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if _, err := conn.Write([]byte{'S'}); err != nil {
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{backendCert}})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	if readBeforeClose > 0 {
		buf := make([]byte, readBeforeClose)
		if _, err := io.ReadFull(tlsConn, buf); err != nil {
			return
		}
	}
	// Close the raw pipe directly, not tlsConn: tls.Conn.Close() attempts a
	// close_notify handshake, which would block waiting for the proxy to
	// read it — but the proxy is about to be blocked *writing* its next
	// message to this same conn. Neither side would ever read, deadlocking
	// both. Closing the raw conn instead fails the proxy's pending/next
	// write immediately, which is what this helper exists to trigger.
	conn.Close()
}

// TestRelease_BackendHeaderWriteFailure is the mutation-provable test for
// handler.go's Release call after the forwarded StartupMessage *header*
// write to the backend fails (the backend accepted TLS but closed before
// reading anything).
func TestRelease_BackendHeaderWriteFailure(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	assertNoCapacityLeak(t, 3,
		func(reg *registry.InMemoryRegistry) *Handler {
			return &Handler{
				Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
				Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
				Registry:   reg,
				Relay:      relay.New(0),
				Audit:      audit.NewSink(io.Discard),
				Metrics:    metrics.New(),
				ClientTLS: &tls.Config{
					Certificates: []tls.Certificate{clientCert},
					MinVersion:   tls.VersionTLS12,
				},
				BackendTLS: &tls.Config{RootCAs: certPool(t, backendCADER)},
				Dial: func(ctx context.Context, addr string) (net.Conn, error) {
					proxySide, backendSide := net.Pipe()
					go runBackendThatFailsAfterNBytes(t, backendSide, backendCert, 0) // closes before reading the header
					return proxySide, nil
				},
				Timeouts: Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
			}
		},
		func(h *Handler) {
			clientSide, proxyClientSide := net.Pipe()
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
			io.Copy(io.Discard, tlsConn)
			tlsConn.Close()
			<-done
		})
}

// TestRelease_BackendPayloadWriteFailure is the mutation-provable test for
// handler.go's Release call after the forwarded StartupMessage *payload*
// write to the backend fails (the header write succeeded, but the backend
// closes before the payload arrives).
func TestRelease_BackendPayloadWriteFailure(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	assertNoCapacityLeak(t, 3,
		func(reg *registry.InMemoryRegistry) *Handler {
			return &Handler{
				Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
				Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
				Registry:   reg,
				Relay:      relay.New(0),
				Audit:      audit.NewSink(io.Discard),
				Metrics:    metrics.New(),
				ClientTLS: &tls.Config{
					Certificates: []tls.Certificate{clientCert},
					MinVersion:   tls.VersionTLS12,
				},
				BackendTLS: &tls.Config{RootCAs: certPool(t, backendCADER)},
				Dial: func(ctx context.Context, addr string) (net.Conn, error) {
					proxySide, backendSide := net.Pipe()
					// Reads exactly the 8-byte forwarded header, then closes
					// before the payload arrives.
					go runBackendThatFailsAfterNBytes(t, backendSide, backendCert, pgwire.HeaderSize)
					return proxySide, nil
				},
				Timeouts: Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
			}
		},
		func(h *Handler) {
			clientSide, proxyClientSide := net.Pipe()
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
			io.Copy(io.Discard, tlsConn)
			tlsConn.Close()
			<-done
		})
}
