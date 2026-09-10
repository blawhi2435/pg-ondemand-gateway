package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// listenTLSBackend starts a bare TCP listener that answers a single
// SSLRequest with 'S' and then completes a TLS server handshake using
// cert, for exactly one connection.
func listenTLSBackend(t *testing.T, cert tls.Certificate) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if _, err := conn.Write([]byte{'S'}); err != nil {
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		tlsConn.Handshake() // best-effort; the client side observes success/failure
	}()
	return ln
}

// TestDialBackend_TrustsCertByCAEvenWhenHostnameDoesNotMatchDialAddr is the
// round-3 regression test for a real integration gap this round's e2e
// testing found: a CNPG Pooler's client-facing TLS certificate is its
// owning Cluster's own server certificate (SAN covers <cluster>-rw/-ro/-r),
// not the Pooler's own Service name — so dialBackend's previous default of
// ServerName = hostOf(addr) always failed hostname verification against a
// real Pooler, even though the certificate chains to the trusted CA fine.
// CNPG's own PgBouncer config makes exactly the same trade for its
// backend connection to Postgres (server_tls_sslmode=verify-ca, not
// verify-full) — pg-proxy matches that: trust the chain against RootCAs,
// skip the hostname check.
func TestDialBackend_TrustsCertByCAEvenWhenHostnameDoesNotMatchDialAddr(t *testing.T) {
	// The cert's own identity ("wrong-name.example") deliberately does not
	// match the dial address's hostname ("127.0.0.1") — mirroring the real
	// CNPG Pooler-vs-Cluster-hostname mismatch.
	cert, der := genTestCert(t, "wrong-name.example")
	ln := listenTLSBackend(t, cert)
	defer ln.Close()

	tlsCfg := &tls.Config{RootCAs: certPool(t, der)}
	conn, err := dialBackend(context.Background(), dialTCP, ln.Addr().String(), tlsCfg, 2*time.Second)
	if err != nil {
		t.Fatalf("dialBackend: %v, want success — the cert is trusted via RootCAs even though its DNSName doesn't match the dial address", err)
	}
	conn.Close()
}

// TestDialBackend_StillRejectsUntrustedCA is the paired negative case:
// trusting the chain instead of the hostname must not mean trusting
// everything — a cert from a CA outside RootCAs must still be rejected.
func TestDialBackend_StillRejectsUntrustedCA(t *testing.T) {
	presentedCert, _ := genTestCert(t, "backend.internal")
	_, trustedDER := genTestCert(t, "some-other-name") // a *different* cert/CA, unrelated to presentedCert
	ln := listenTLSBackend(t, presentedCert)
	defer ln.Close()

	tlsCfg := &tls.Config{RootCAs: certPool(t, trustedDER)}
	conn, err := dialBackend(context.Background(), dialTCP, ln.Addr().String(), tlsCfg, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("dialBackend: want an error for a certificate signed by an untrusted CA, got success")
	}
}
