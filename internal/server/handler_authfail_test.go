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

// TestHandleConn_BackendClosesWithoutAuthenticationOk is the round-2
// critical regression test: the most common real backend-auth failure
// (wrong password, pg_hba rejection) has the backend send an ErrorResponse
// and close *without* ever sending AuthenticationOk. authOkWatcher.Detected
// only ever fired on the success pattern, so relayAfterAuth's registration
// goroutine parked in its select forever, HandleConn never returned, the
// connection was never untracked from the Listener, and no disconnect
// event was ever written.
func TestHandleConn_BackendClosesWithoutAuthenticationOk(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	var auditBuf bytes.Buffer
	reg := registry.NewInMemoryRegistry(fixedLimits(0, 0))
	h := &Handler{
		Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
		Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
		Registry:   reg,
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

		// The realistic failure: an ErrorResponse instead of
		// AuthenticationOk, then close — no auth-ok pattern ever crosses.
		tlsConn.Write(pgwire.NewErrorResponse("28P01", "password authentication failed"))
		tlsConn.Close()
	}()

	clientSide.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	// Drain whatever the proxy relays (the ErrorResponse) so the relay's
	// own close-notify exchange doesn't stall the deadline-free flow.
	go drainAndClose(tlsConn)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return after the backend closed without ever sending AuthenticationOk")
	}

	total, _ := reg.Count()
	if total != 0 {
		t.Errorf("registry has %d entries; a connection that never authenticated must not stay registered", total)
	}

	logged := auditBuf.String()
	if !strings.Contains(logged, `"event":"disconnect"`) {
		t.Errorf("no disconnect event was emitted for a connection that failed backend authentication:\n%s", logged)
	}
	if strings.Contains(logged, `"event":"connect"`) {
		t.Errorf("a connect event must not be emitted for a connection that never received AuthenticationOk:\n%s", logged)
	}

	// A connection that never authenticated must never have incremented
	// connections_active in the first place — no time series for this
	// cluster should exist at all (Gather() only ever reports labels that
	// were actually observed).
	families, _ := h.Metrics.Registry().Gather()
	for _, fam := range families {
		if fam.GetName() != "pgproxy_connections_active" {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatchExact(m.GetLabel(), map[string]string{"cluster": "tenant1"}) {
				t.Errorf("pgproxy_connections_active{cluster=tenant1} = %v, want no series at all", m.Gauge.GetValue())
			}
		}
	}
}
