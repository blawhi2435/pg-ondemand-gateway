package server

import (
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

func trustedNet(t *testing.T, cidr string) []*net.IPNet {
	t.Helper()
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return []*net.IPNet{ipnet}
}

// TestHandleConn_ProxyProtocol_ClientIPFromHeader is the failing test for
// task 24.1: with proxyProtocol enabled and the real TCP peer on the
// trusted allowlist, the PROXY v1 header's declared source IP — not the
// TCP peer address — must be what reaches Authorizer.Check and the audit
// log's client_ip.
func TestHandleConn_ProxyProtocol_ClientIPFromHeader(t *testing.T) {
	backendCert, backendCADER := genTestCert(t, "backend.internal")
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")

	ln := newLoopbackListener(t)
	addr := ln.Addr().String()

	proxyBackendSide, backendSide := net.Pipe()

	var observedClientIP string
	h := &Handler{
		Router: fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}},
		Authorizer: authzFunc(func(cluster, user, database, clientIP string) authz.Decision {
			observedClientIP = clientIP
			return authz.Decision{Allow: true}
		}),
		Registry: registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		Relay:    relay.New(0),
		Audit:    audit.NewSink(io.Discard),
		Metrics:  metrics.New(),
		ClientTLS: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
		BackendTLS: &tls.Config{RootCAs: certPool(t, backendCADER)},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return proxyBackendSide, nil
		},
		Timeouts:             Timeouts{Handshake: 5 * time.Second, BackendDial: 5 * time.Second, Idle: 0},
		ProxyProtocolEnabled: true,
		TrustedProxyNets:     trustedNet(t, "127.0.0.1/32"), // the real TCP dialer below
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.HandleConn(context.Background(), conn)
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

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// PROXY v1 header declaring a source IP that is NOT the real TCP peer.
	conn.Write([]byte("PROXY TCP4 203.0.113.7 198.51.100.9 51234 5432\r\n"))
	conn.Write(sslRequestPacket())
	reply := make([]byte, 1)
	io.ReadFull(conn, reply)
	if reply[0] != 'S' {
		t.Fatalf("SSLRequest reply = %q, want 'S'", reply[0])
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	authOk := make([]byte, len(authOkBytes))
	tlsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(tlsConn, authOk); err != nil {
		t.Fatalf("read AuthenticationOk: %v", err)
	}
	drainAndClose(tlsConn)

	time.Sleep(50 * time.Millisecond) // let Authorizer.Check's assignment land
	if observedClientIP != "203.0.113.7" {
		t.Errorf("Authorizer.Check clientIP = %q, want %q (from the PROXY header)", observedClientIP, "203.0.113.7")
	}
}

// TestHandleConn_ProxyProtocol_UntrustedSourceRejected verifies the
// security-review requirement that accompanies 24.2: a PROXY header is
// honored only from an allow-listed source. An untrusted peer must be
// rejected outright when proxyProtocol is enabled — not silently ignored
// (which would let anyone spoof client_ip) and not passed through as if
// the feature were off.
func TestHandleConn_ProxyProtocol_UntrustedSourceRejected(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")

	ln := newLoopbackListener(t)
	addr := ln.Addr().String()

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
		BackendTLS:           &tls.Config{},
		Timeouts:             Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
		ProxyProtocolEnabled: true,
		TrustedProxyNets:     trustedNet(t, "10.0.0.0/8"), // does not cover 127.0.0.1
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.HandleConn(context.Background(), conn)
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("PROXY TCP4 203.0.113.7 198.51.100.9 51234 5432\r\n"))
	conn.Write(sslRequestPacket())

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("connection from an untrusted source got a reply (%q); want it rejected/closed with no data sent", buf[:n])
	}
}

// TestHandleConn_ProxyProtocol_UntrustedSourceMetric is the round-3
// regression test for task 28.5: an untrusted PROXY-protocol source and a
// malformed PROXY header were both counted under the same generic
// "bad_startup" reason, indistinguishable from any other malformed first
// packet in pgproxy_handshake_errors_total. An operator debugging a
// misconfigured trustedCIDRs allowlist needs the two distinguished.
func TestHandleConn_ProxyProtocol_UntrustedSourceMetric(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")

	ln := newLoopbackListener(t)
	addr := ln.Addr().String()

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
		BackendTLS:           &tls.Config{},
		Timeouts:             Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
		ProxyProtocolEnabled: true,
		TrustedProxyNets:     trustedNet(t, "10.0.0.0/8"), // does not cover 127.0.0.1
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.HandleConn(context.Background(), conn)
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("PROXY TCP4 203.0.113.7 198.51.100.9 51234 5432\r\n"))
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "untrusted_proxy_source"}, 1) {
		t.Error(`handshake_errors_total{reason="untrusted_proxy_source"} was not incremented for a peer outside trustedCIDRs`)
	}
}

// TestHandleConn_ProxyProtocol_BadHeaderMetric is the paired case: a
// trusted source sending a malformed PROXY header must be counted under
// "bad_proxy_header", distinct from "untrusted_proxy_source".
func TestHandleConn_ProxyProtocol_BadHeaderMetric(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")

	ln := newLoopbackListener(t)
	addr := ln.Addr().String()

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
		BackendTLS:           &tls.Config{},
		Timeouts:             Timeouts{Handshake: 2 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
		ProxyProtocolEnabled: true,
		TrustedProxyNets:     trustedNet(t, "127.0.0.1/32"),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		h.HandleConn(context.Background(), conn)
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("NOT A VALID PROXY HEADER\r\n"))
	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "bad_proxy_header"}, 1) {
		t.Error(`handshake_errors_total{reason="bad_proxy_header"} was not incremented for a malformed PROXY header from a trusted source`)
	}
}
