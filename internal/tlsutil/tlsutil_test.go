package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genCert creates a self-signed cert/key PEM pair for tests, with a
// distinguishing serial number and NotAfter so reloads are observable.
func genCert(t *testing.T, serial int64, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "*.db.test"},
		DNSNames:     []string{"*.db.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writeCertFiles(t *testing.T, dir string, certPEM, keyPEM []byte) (certFile, keyFile string) {
	t.Helper()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func TestStore_GetCertificate_ReturnsLoadedCert(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(24 * time.Hour)
	certPEM, keyPEM := genCert(t, 1, notAfter)
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cert, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert.Leaf == nil || cert.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("GetCertificate returned unexpected cert: %+v", cert.Leaf)
	}
}

func TestStore_ReloadNow_NewCertTakesEffect(t *testing.T) {
	dir := t.TempDir()
	certPEM1, keyPEM1 := genCert(t, 1, time.Now().Add(24*time.Hour))
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	certPEM2, keyPEM2 := genCert(t, 2, time.Now().Add(48*time.Hour))
	writeCertFiles(t, dir, certPEM2, keyPEM2)

	changed, err := store.ReloadNow()
	if err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if !changed {
		t.Fatal("ReloadNow reported changed=false after cert content changed")
	}

	cert, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("GetCertificate returned serial %d, want 2 (the new cert)", cert.Leaf.SerialNumber.Int64())
	}
}

func TestStore_ReloadNow_BadCertDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := genCert(t, 1, time.Now().Add(24*time.Hour))
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("corrupt cert file: %v", err)
	}

	changed, err := store.ReloadNow()
	if err == nil {
		t.Fatal("ReloadNow: want error for an unparseable certificate, got nil")
	}
	if changed {
		t.Error("ReloadNow reported changed=true for a bad certificate")
	}

	cert, err := store.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after bad reload: %v", err)
	}
	if cert.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("GetCertificate returned serial %d, want 1 (the last-good cert)", cert.Leaf.SerialNumber.Int64())
	}
}

func TestStore_GetCertificate_AlwaysReadsCurrentValue(t *testing.T) {
	dir := t.TempDir()
	certPEM1, keyPEM1 := genCert(t, 1, time.Now().Add(24*time.Hour))
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	first, _ := store.GetCertificate(nil)
	if first.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("first GetCertificate = serial %d, want 1", first.Leaf.SerialNumber.Int64())
	}

	certPEM2, keyPEM2 := genCert(t, 2, time.Now().Add(24*time.Hour))
	writeCertFiles(t, dir, certPEM2, keyPEM2)
	if _, err := store.ReloadNow(); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}

	// The connection that already has `first` is unaffected (it's a plain
	// struct value, not re-read); a *new* call gets the new one — this is
	// what makes existing connections immune to a cert rotation.
	second, _ := store.GetCertificate(nil)
	if second.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("second GetCertificate = serial %d, want 2", second.Leaf.SerialNumber.Int64())
	}
	if first.Leaf.SerialNumber.Int64() != 1 {
		t.Fatal("the earlier returned certificate value must not mutate in place")
	}
}

// TestStore_ExistingConnectionSurvivesCertRotation is the round-2
// regression test for config-hot-reload Scenario「既有連線不受影響」: a
// connection established before rotation must keep working — not just
// "the pointer value didn't mutate in place" (a property true of any
// atomic.Pointer-based implementation, proven already above), but an
// actual live TLS connection continuing to round-trip data and still
// presenting its original certificate after the store rotates.
func TestStore_ExistingConnectionSurvivesCertRotation(t *testing.T) {
	dir := t.TempDir()
	certPEM1, keyPEM1 := genCert(t, 1, time.Now().Add(24*time.Hour))
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		srv := tls.Server(serverConn, &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: store.GetCertificate,
		})
		defer serverConn.Close() // close the raw pipe directly: no close_notify handshake to deadlock on
		if err := srv.Handshake(); err != nil {
			serverDone <- err
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(srv, buf); err != nil {
			serverDone <- err
			return
		}
		if _, err := srv.Write(buf); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	pool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(mustDER(t, certPEM1))
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool.AddCert(leaf)
	cli := tls.Client(clientConn, &tls.Config{ServerName: "*.db.test", RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err := cli.Handshake(); err != nil {
		t.Fatalf("client handshake before rotation: %v", err)
	}
	if got := cli.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != 1 {
		t.Fatalf("peer certificate serial = %d, want 1", got)
	}

	// Rotate while this connection is live.
	certPEM2, keyPEM2 := genCert(t, 2, time.Now().Add(24*time.Hour))
	writeCertFiles(t, dir, certPEM2, keyPEM2)
	if _, err := store.ReloadNow(); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}

	// The already-established connection must still work — no
	// renegotiation, no interruption — and still be serial 1.
	if _, err := cli.Write([]byte("ping")); err != nil {
		t.Fatalf("write on the pre-rotation connection failed: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(cli, echo); err != nil {
		t.Fatalf("read on the pre-rotation connection failed: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q, want ping", echo)
	}
	if got := cli.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != 1 {
		t.Fatalf("peer certificate serial after rotation = %d, want still 1 (no re-handshake)", got)
	}

	// Check the server's outcome before closing — Close() would otherwise
	// block sending its own close_notify, since the server has already
	// finished reading and won't consume it until it also closes.
	if err := <-serverDone; err != nil {
		t.Fatalf("server side: %v", err)
	}
	clientConn.Close() // close the raw pipe directly: no close_notify handshake to deadlock on
}

func mustDER(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("pem.Decode returned nil")
	}
	return block.Bytes
}

// TestNewStoreWithInterval_RunUsesConfiguredInterval is the round-2
// regression test for design §8.1's tls.reloadInterval: NewStore hardcoded
// DefaultReloadInterval (30s) regardless of what the ConfigMap set, so
// Store.Run's ticker never reflected an operator's configured cadence. A
// 30s-only Run loop would never complete this test's short window.
func TestNewStoreWithInterval_RunUsesConfiguredInterval(t *testing.T) {
	dir := t.TempDir()
	certPEM1, keyPEM1 := genCert(t, 1, time.Now().Add(24*time.Hour))
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	store, err := NewStoreWithInterval(certFile, keyFile, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewStoreWithInterval: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.Run(ctx)

	certPEM2, keyPEM2 := genCert(t, 2, time.Now().Add(24*time.Hour))
	writeCertFiles(t, dir, certPEM2, keyPEM2)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cert, err := store.GetCertificate(nil)
		if err == nil && cert.Leaf.SerialNumber.Int64() == 2 {
			return // Run picked up the change well within the configured interval
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Store.Run did not reload within its configured interval (still stuck on the 30s default?)")
}

func TestStore_NotAfter_MatchesCertificate(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	certPEM, keyPEM := genCert(t, 1, notAfter)
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	store, err := NewStore(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got := store.NotAfter()
	if !got.Equal(notAfter) {
		t.Fatalf("NotAfter() = %v, want %v", got, notAfter)
	}
}
