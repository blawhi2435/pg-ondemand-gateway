package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
)

func cancelRequestPacket(pid, secret uint32) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:4], 16)
	binary.BigEndian.PutUint32(buf[4:8], 80877102)
	binary.BigEndian.PutUint32(buf[8:12], pid)
	binary.BigEndian.PutUint32(buf[12:16], secret)
	return buf
}

// TestHandleConn_CancelRequest_RateLimited is the round-2 regression test
// for the security review's finding that CancelRequest had no resource
// ceiling of any kind: past the per-source-IP cap, the backend must not be
// dialed at all.
func TestHandleConn_CancelRequest_RateLimited(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")

	var dialCount atomic.Int32
	h := &Handler{
		Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
		Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
		Registry:   registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		Relay:      relay.New(0),
		Audit:      audit.NewSink(io.Discard),
		Metrics:    metrics.New(),
		ClientTLS: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
		BackendTLS: &tls.Config{},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			dialCount.Add(1)
			client, server := net.Pipe()
			go func() {
				// Play just enough of the backend side to let handleCancel's
				// write succeed and return promptly.
				header := make([]byte, 8)
				io.ReadFull(server, header)
				server.Write([]byte{'S'})
				server.Close()
			}()
			return client, nil
		},
		Timeouts: Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
		// net.Pipe's RemoteAddr string ("pipe") is identical for every
		// connection, so every CancelRequest here shares one source IP —
		// exactly what's needed to exercise the per-IP cap deterministically.
		CancelLimiter: NewCancelRateLimiter(fixedCancelLimits(time.Minute, 2, 0)),
	}

	sendOneCancel := func() {
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
		tlsConn.Write(cancelRequestPacket(1, 1))
		drainAndClose(tlsConn)
		<-done
	}

	sendOneCancel()
	sendOneCancel()
	sendOneCancel() // over the cap of 2

	if got := dialCount.Load(); got != 2 {
		t.Fatalf("backend dialed %d times, want exactly 2 (the 3rd cancel must be rate-limited)", got)
	}

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "cancel_rate_limited"}, 1) {
		t.Error("handshake_errors_total{reason=cancel_rate_limited} was not incremented")
	}
}

func TestHandleConn_CancelRequest_NotRegisteredNoAuthorizerCancelAudited(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	var authorizerCalled atomic.Bool
	authorizer := authzFunc(func(cluster, user, database, clientIP string) authz.Decision {
		authorizerCalled.Store(true)
		return authz.Decision{Allow: true}
	})

	var auditBuf bytes.Buffer
	reg := registry.NewInMemoryRegistry(fixedLimits(0, 0))

	handler := &Handler{
		Router:     fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
		Authorizer: authorizer,
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
		handler.HandleConn(context.Background(), proxyClientSide)
	}()

	backendDone := make(chan struct{})
	var receivedCancel []byte
	go func() {
		defer close(backendDone)
		header := make([]byte, 8)
		io.ReadFull(backendSide, header)
		backendSide.Write([]byte{'S'})
		tlsConn := tls.Server(backendSide, &tls.Config{Certificates: []tls.Certificate{backendCert}})
		if err := tlsConn.Handshake(); err != nil {
			t.Errorf("backend: TLS handshake: %v", err)
			return
		}
		packet := make([]byte, 16)
		if _, err := io.ReadFull(tlsConn, packet); err != nil {
			t.Errorf("backend: read forwarded CancelRequest: %v", err)
			return
		}
		receivedCancel = packet
		drainAndClose(tlsConn)
	}()

	// Client side: SSLRequest, TLS handshake, then CancelRequest — no
	// StartupMessage at all, per design's CancelRequest path.
	if _, err := clientSide.Write(sslRequestPacket()); err != nil {
		t.Fatalf("client: write SSLRequest: %v", err)
	}
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)
	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client: TLS handshake: %v", err)
	}
	if _, err := tlsConn.Write(cancelRequestPacket(4242, 999)); err != nil {
		t.Fatalf("client: write CancelRequest: %v", err)
	}
	drainAndClose(tlsConn)

	<-done
	<-backendDone

	if authorizerCalled.Load() {
		t.Error("Authorizer.Check must not be called for a CancelRequest")
	}
	total, _ := reg.Count()
	if total != 0 {
		t.Errorf("registry has %d entries; CancelRequest must never be registered", total)
	}
	if len(receivedCancel) != 16 {
		t.Fatalf("backend received %d bytes, want the full 16-byte CancelRequest packet", len(receivedCancel))
	}
	if binary.BigEndian.Uint32(receivedCancel[8:12]) != 4242 {
		t.Errorf("forwarded pid = %d, want 4242", binary.BigEndian.Uint32(receivedCancel[8:12]))
	}

	logged := auditBuf.String()
	if !strings.Contains(logged, `"event":"cancel"`) {
		t.Errorf("audit log missing cancel event:\n%s", logged)
	}
	if strings.Contains(logged, `"event":"connect"`) || strings.Contains(logged, `"event":"disconnect"`) {
		t.Errorf("CancelRequest must not produce connect/disconnect events:\n%s", logged)
	}
}

// TestHandleConn_CancelRequest_RateLimited_IsAudited is the round-3
// regression test for the finding that a rate-limited CancelRequest
// incremented a metric but left no audit trail at all — an aggregate
// counter can't answer "which client IP got throttled and when", the
// exact question an operator investigating a rate-limit incident needs
// answered. A rejected cancel must be both metered and audited, never
// silently dropped.
func TestHandleConn_CancelRequest_RateLimited_IsAudited(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")

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
		BackendTLS: &tls.Config{},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				header := make([]byte, 8)
				io.ReadFull(server, header)
				server.Write([]byte{'S'})
				server.Close()
			}()
			return client, nil
		},
		Timeouts:      Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
		CancelLimiter: NewCancelRateLimiter(fixedCancelLimits(time.Minute, 0, 0)), // MaxPerIP=0 disabled, MaxInFlight=0 disabled — force via a pre-consumed slot below
	}
	// Force the very first Allow to fail deterministically regardless of
	// limiter internals: cap in-flight at 1 and hold the slot open.
	h.CancelLimiter = NewCancelRateLimiter(fixedCancelLimits(time.Minute, 0, 1))
	h.CancelLimiter.Allow("occupied") // consume the single in-flight slot, held for the test

	sendOneCancel := func() {
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
		tlsConn.Write(cancelRequestPacket(1, 1))
		drainAndClose(tlsConn)
		<-done
	}

	sendOneCancel()

	logged := auditBuf.String()
	if !strings.Contains(logged, `"event":"cancel_rejected"`) {
		t.Errorf("audit log missing cancel_rejected event for a rate-limited cancel:\n%s", logged)
	}
	if !strings.Contains(logged, `"reason":"cancel_rate_limited"`) {
		t.Errorf("audit log missing reason=cancel_rate_limited:\n%s", logged)
	}
}
