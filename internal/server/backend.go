package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
)

// Dialer opens the raw (pre-TLS) connection to a backend address. Tests
// substitute a fake that hands back one end of a net.Pipe instead of
// dialing real TCP.
type Dialer func(ctx context.Context, addr string) (net.Conn, error)

// dialTCP is the production Dialer.
func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// sslRequestBytes is the fixed 8-byte SSLRequest packet (design §6.1).
var sslRequestBytes = func() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], 80877103)
	return buf
}()

// dialBackend connects to addr and negotiates standard PostgreSQL TLS: send
// SSLRequest, expect 'S', then a normal TLS handshake verified against
// tlsCfg's RootCAs (design §6.4 — never direct TLS, which would additionally
// require PgBouncer >= 1.25).
func dialBackend(ctx context.Context, dial Dialer, addr string, tlsCfg *tls.Config, timeout time.Duration) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := dial(dialCtx, addr)
	if err != nil {
		return nil, fmt.Errorf("server: dial backend %s: %w", addr, err)
	}
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}

	if _, err := raw.Write(sslRequestBytes); err != nil {
		raw.Close()
		return nil, fmt.Errorf("server: send SSLRequest to backend %s: %w", addr, err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(raw, reply); err != nil {
		raw.Close()
		return nil, fmt.Errorf("server: read SSLRequest reply from backend %s: %w", addr, err)
	}
	if reply[0] != 'S' {
		raw.Close()
		return nil, fmt.Errorf("server: backend %s refused TLS (replied %q)", addr, reply[0])
	}

	cfg := tlsCfg.Clone()
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		// A CNPG Pooler presents its owning Cluster's own server
		// certificate (SAN covers <cluster>-rw/-ro/-r), not the Pooler's
		// own Service name pg-proxy actually dials — so defaulting
		// ServerName to hostOf(addr) here would fail hostname
		// verification against every real Pooler, cert validity and CA
		// trust notwithstanding. CNPG's own PgBouncer config makes the
		// identical trade for ITS backend connection to Postgres
		// (server_tls_sslmode=verify-ca, not verify-full): trust the
		// chain against RootCAs, skip the hostname check. A caller that
		// sets ServerName explicitly still gets full verify-full; a
		// caller that already set InsecureSkipVerify (test-only) is left
		// exactly as insecure as it asked to be, not upgraded to this
		// chain check underneath it.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = verifyChainOnly(cfg.RootCAs)
	}
	tlsConn := tls.Client(raw, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("server: TLS handshake with backend %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Time{}) // clear the dial deadline; relay owns idle timeout from here
	return tlsConn, nil
}

// verifyChainOnly builds a tls.Config.VerifyConnection callback that
// verifies the peer's certificate chains to roots — CA trust, exactly like
// Go's normal verification — without checking the presented ServerName
// against the certificate's DNS names. Used only when the caller left
// ServerName unset, i.e. explicitly asked for CA-trust-only verification
// (see dialBackend).
func verifyChainOnly(roots *x509.CertPool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("server: no peer certificate presented")
		}
		intermediates := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
}

// hostOf strips the port from a host:port address for use as a TLS
// ServerName when the caller hasn't pinned one explicitly.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// encodeHeader re-serializes a pgwire.Header to its 8-byte wire form. This
// is lossless — the header carries no information beyond length and code —
// so it's safe to rebuild rather than needing to keep the original bytes
// around from ReadHeader.
func encodeHeader(h pgwire.Header) []byte {
	buf := make([]byte, pgwire.HeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], uint32(h.Length))
	binary.BigEndian.PutUint32(buf[4:8], uint32(h.Code))
	return buf
}
