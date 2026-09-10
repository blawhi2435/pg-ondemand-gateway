package server

import (
	"bytes"
	"context"
	"crypto/tls"
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

// TestHandleConn_TransmitsAfterHandshakeDeadlineElapses is the regression
// test for the round-2 critical: HandleConn set conn.SetDeadline (both read
// AND write) for the handshake phase and never cleared it before handing
// the client socket to Relay.Run. relay.deadlineReader only ever refreshes
// the *read* deadline, so the client's write deadline stayed frozen at
// connect_time+handshake — every backend->client write failed, and
// mid-TLS-record failures got misclassified as idle_timeout.
func TestHandleConn_TransmitsAfterHandshakeDeadlineElapses(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	const handshakeTimeout = 50 * time.Millisecond
	const pastDeadlineSleep = 300 * time.Millisecond // comfortably past handshakeTimeout

	h := &Handler{
		Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
		Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
		Registry:   registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		Relay:      relay.New(0), // idle disabled: isolate the handshake-deadline bug specifically
		Audit:      audit.NewSink(&bytes.Buffer{}),
		Metrics:    metrics.New(),
		ClientTLS: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
		BackendTLS: &tls.Config{RootCAs: certPool(t, backendCADER)},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return proxyBackendSide, nil
		},
		Timeouts: Timeouts{Handshake: handshakeTimeout, BackendDial: 5 * time.Second, Idle: 0},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	backendErrs := make(chan error, 1)
	go func() {
		backendErrs <- func() error {
			header := make([]byte, 8)
			if _, err := io.ReadFull(backendSide, header); err != nil {
				return err
			}
			if _, err := backendSide.Write([]byte{'S'}); err != nil {
				return err
			}
			tlsConn := tls.Server(backendSide, &tls.Config{Certificates: []tls.Certificate{backendCert}})
			if err := tlsConn.Handshake(); err != nil {
				return err
			}

			startupHeader, err := pgwire.ReadHeader(tlsConn)
			if err != nil {
				return err
			}
			payload := make([]byte, int(startupHeader.Length)-pgwire.HeaderSize)
			if _, err := io.ReadFull(tlsConn, payload); err != nil {
				return err
			}

			if _, err := tlsConn.Write([]byte(authOkBytes)); err != nil {
				return err
			}

			// The critical bug: the proxy's client-side write deadline was
			// frozen at connect+handshakeTimeout. Sleeping well past it
			// before writing more data is what exposes the bug.
			time.Sleep(pastDeadlineSleep)

			if _, err := tlsConn.Write([]byte("LATE")); err != nil {
				return err
			}

			ping := make([]byte, 4)
			if _, err := io.ReadFull(tlsConn, ping); err != nil {
				return err
			}
			if string(ping) != "PING" {
				return err
			}
			tlsConn.Close()
			return nil
		}()
	}()

	if _, err := clientSide.Write(sslRequestPacket()); err != nil {
		t.Fatalf("client: write SSLRequest: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(clientSide, reply); err != nil {
		t.Fatalf("client: read SSLRequest reply: %v", err)
	}
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client: TLS handshake: %v", err)
	}
	if _, err := tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"})); err != nil {
		t.Fatalf("client: write StartupMessage: %v", err)
	}

	authOk := make([]byte, len(authOkBytes))
	if _, err := io.ReadFull(tlsConn, authOk); err != nil {
		t.Fatalf("client: read AuthenticationOk: %v", err)
	}

	// This is the read that fails today: the backend's write, issued after
	// pastDeadlineSleep, gets silently truncated/rejected by the proxy's
	// stale write deadline before it ever reaches the client.
	late := make([]byte, 4)
	tlsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(tlsConn, late); err != nil {
		t.Fatalf("client: read data written by the backend after the handshake deadline elapsed: %v", err)
	}
	if string(late) != "LATE" {
		t.Fatalf("client: got %q, want %q", late, "LATE")
	}

	if _, err := tlsConn.Write([]byte("PING")); err != nil {
		t.Fatalf("client: write PING after the handshake deadline elapsed: %v", err)
	}

	select {
	case err := <-backendErrs:
		if err != nil {
			t.Fatalf("backend: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend goroutine did not finish")
	}

	tlsConn.Close()
	<-done
}
