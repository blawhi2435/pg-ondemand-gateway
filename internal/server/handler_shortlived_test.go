package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
)

// TestHandleConn_ShortLivedConnections_AlwaysProduceMatchingConnectEvent is
// the round-3 end-to-end regression test for task 27.3: a connection that
// authenticates successfully and then closes immediately (a health check,
// `psql -c`, any short script) must always produce a connect event, never
// just a disconnect. Before the awaitAuthOutcome fix, the backend sending
// AuthenticationOk and closing in the same breath raced Detected against
// Closed in a 3-way select with no priority, and Go picked uniformly at
// random — reproduced by the security reviewer at ~50% loss over 3000
// trials. This drives real connections through the actual HandleConn flow
// (real TLS both sides, real StartupMessage forwarding) rather than the
// isolated select logic, so it also catches a regression anywhere in that
// wiring, not just in awaitAuthOutcome itself.
func TestHandleConn_ShortLivedConnections_AlwaysProduceMatchingConnectEvent(t *testing.T) {
	const iterations = 300
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	var connectCount, disconnectCount int
	for i := 0; i < iterations; i++ {
		clientSide, proxyClientSide := net.Pipe()
		proxyBackendSide, backendSide := net.Pipe()

		var auditBuf bytes.Buffer
		h := &Handler{
			Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
			Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
			Registry:   registry.NewInMemoryRegistry(fixedLimits(0, 0)),
			Relay:      relay.New(0),
			Audit:      audit.NewSink(&auditBuf),
			Metrics:    metrics.New(),
			ClientTLS: &tls.Config{
				Certificates: []tls.Certificate{clientCert},
				MinVersion:   tls.VersionTLS12,
			},
			BackendTLS: &tls.Config{RootCAs: certPool(t, backendCADER)},
			Dial: func(ctx context.Context, addr string) (net.Conn, error) {
				return proxyBackendSide, nil
			},
			Timeouts: Timeouts{Handshake: 5 * time.Second, BackendDial: 5 * time.Second, Idle: 0},
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.HandleConn(context.Background(), proxyClientSide)
		}()

		backendDone := make(chan struct{})
		go func() {
			defer close(backendDone)
			header := make([]byte, 8)
			if _, err := io.ReadFull(backendSide, header); err != nil {
				return
			}
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
			if _, err := io.ReadFull(tlsConn, payload); err != nil {
				return
			}
			// The key trigger: write AuthenticationOk and close immediately
			// — a short-lived session that authenticates and hangs up right
			// away, giving the backend->client Read a real chance to
			// observe both the auth-ok bytes and a terminal error together.
			tlsConn.Write([]byte(authOkBytes))
			tlsConn.Close()
		}()

		clientSide.Write(sslRequestPacket())
		reply := make([]byte, 1)
		if _, err := io.ReadFull(clientSide, reply); err != nil {
			t.Fatalf("iteration %d: read SSLRequest reply: %v", i, err)
		}
		tlsConn := tls.Client(clientSide, &tls.Config{
			ServerName: "tenant1.db.test",
			RootCAs:    certPool(t, clientCADER),
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.Handshake(); err != nil {
			t.Fatalf("iteration %d: client TLS handshake: %v", i, err)
		}
		tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))
		// Close immediately after writing the startup message, without
		// waiting to read anything back — the client side of the same
		// "authenticate then hang up right away" scenario.
		io.Copy(io.Discard, tlsConn) // drain whatever the proxy relays until it closes
		tlsConn.Close()

		<-done
		<-backendDone

		logged := auditBuf.String()
		if strings.Contains(logged, `"event":"connect"`) {
			connectCount++
		}
		if strings.Contains(logged, `"event":"disconnect"`) {
			disconnectCount++
		}
	}

	if connectCount != iterations {
		t.Errorf("connect events = %d, want %d (out of %d short-lived authenticated connections)", connectCount, iterations, iterations)
	}
	if disconnectCount != iterations {
		t.Errorf("disconnect events = %d, want %d", disconnectCount, iterations)
	}
	if connectCount != disconnectCount {
		t.Errorf("connect (%d) and disconnect (%d) counts must match — a mismatch means an orphaned disconnect event with no matching connect", connectCount, disconnectCount)
	}
}
