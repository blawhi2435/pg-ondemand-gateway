package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/config"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/route"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/server"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/tlsutil"
)

// --- test fixtures: cert generation, wire-format packets ----------------
//
// These mirror internal/server's and internal/tlsutil's own unexported test
// helpers (genTestCert, sslRequestPacket, buildStartupMessage, ...) — they
// can't be imported across package boundaries, so this is a deliberately
// small, local re-implementation for the one integration test in this file.

func genCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
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

func sslRequestPacket() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], 80877103)
	return buf
}

func buildStartupMessage(params map[string]string) []byte {
	var body bytes.Buffer
	for k, v := range params {
		body.WriteString(k)
		body.WriteByte(0)
		body.WriteString(v)
		body.WriteByte(0)
	}
	body.WriteByte(0)
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], uint32(8+body.Len()))
	binary.BigEndian.PutUint32(header[4:8], 196608)
	return append(header, body.Bytes()...)
}

func cancelRequestPacket(pid, secret uint32) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:4], 16)
	binary.BigEndian.PutUint32(buf[4:8], 80877102)
	binary.BigEndian.PutUint32(buf[8:12], pid)
	binary.BigEndian.PutUint32(buf[12:16], secret)
	return buf
}

const authOkBytes = "\x52\x00\x00\x00\x08\x00\x00\x00\x00" // AuthenticationOk

// fakeRoute is the minimal route.Router this file needs: one fixed
// hostname mapping to one fixed backend address.
type fakeRoute struct {
	sni   string
	route route.Route
}

func (f fakeRoute) Lookup(sni string) (route.Route, bool) {
	if sni == f.sni {
		return f.route, true
	}
	return route.Route{}, false
}

// runFakeBackend accepts exactly one TCP connection on ln, completes the
// standard-TLS backend handshake (SSLRequest -> 'S' -> TLS server
// handshake), reads the forwarded StartupMessage, and writes
// AuthenticationOk — mirroring dialBackend/handleStartup's expectations
// closely enough for a real, successfully-authenticated connection to flow
// all the way through the real Listener under test.
func runFakeBackend(t *testing.T, ln net.Listener, backendCertPEM, backendKeyPEM []byte) {
	t.Helper()
	cert, err := tls.X509KeyPair(backendCertPEM, backendKeyPEM)
	if err != nil {
		t.Fatalf("backend: load cert: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if _, err := conn.Write([]byte{'S'}); err != nil {
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		startupHeader := make([]byte, 8)
		if _, err := io.ReadFull(tlsConn, startupHeader); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(startupHeader[0:4])
		payload := make([]byte, int(length)-8)
		if _, err := io.ReadFull(tlsConn, payload); err != nil {
			return
		}
		if _, err := tlsConn.Write([]byte(authOkBytes)); err != nil {
			return
		}
		// Keep writing a heartbeat byte so the client side can positively
		// observe "the relay is still flowing data" with a bounded Read,
		// rather than inferring liveness from a single Write succeeding —
		// a Write into a TCP send buffer can succeed for a short while
		// even after the peer already closed, masking exactly the
		// instant-force-close regression this test exists to catch.
		go func() {
			for {
				time.Sleep(15 * time.Millisecond)
				if _, err := tlsConn.Write([]byte("H")); err != nil {
					return
				}
			}
		}()
		io.Copy(io.Discard, tlsConn)
	}()
}

// buildTestDeps assembles a *deps by hand — not via buildDeps, which needs
// a real (or fake) k8s API server for its InformerRouter — wiring in the
// exact same production functions (limitsFrom, cancelLimitsFrom,
// buildHandler) buildDeps itself uses, so this test exercises the real
// wiring rather than a hand-rolled substitute for it.
func buildTestDeps(t *testing.T, cfgYAML string, backendAddr string, sni string, cluster string) (*deps, string) {
	t.Helper()
	dir := t.TempDir()

	clientCertPEM, clientKeyPEM := genCert(t, sni)
	certFile, keyFile := writeCertFiles(t, dir, clientCertPEM, clientKeyPEM)

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfgStore, err := config.NewStore(cfgPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := cfgStore.Current()

	certStore, err := tlsutil.NewStoreWithInterval(certFile, keyFile, cfg.TLS.ReloadInterval.Duration())
	if err != nil {
		t.Fatalf("tlsutil.NewStoreWithInterval: %v", err)
	}

	router := route.NewInformerRouter(nil, route.InformerConfig{}, nil) // never Start()ed — see pollGauges's 10s ticker note below
	met := metrics.New()
	limits := limitsFrom(cfgStore)
	cancelLimits := cancelLimitsFrom(cfgStore)

	handler := buildHandler(cfg, router, certStore, x509.NewCertPool(), met, limits, cancelLimits)
	handler.Router = fakeRoute{sni: sni, route: route.Route{Cluster: cluster, Addr: backendAddr}}
	handler.Authorizer = authz.NewAllowAll()
	handler.Registry = registry.NewInMemoryRegistry(func() registry.Limits {
		return registry.Limits{MaxConns: 100, MaxConnsPerCluster: 100}
	})
	handler.Relay = relay.New(cfg.Timeouts.Idle.Duration())
	handler.Audit = audit.NewSink(io.Discard)
	handler.BackendTLS = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only: real cert verification is exercised elsewhere

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	prober := metrics.NewProber(func() bool { return true })
	metricsSrv := metrics.NewServer("127.0.0.1:0", met, prober)

	d := &deps{
		cfgStore:   cfgStore,
		certStore:  certStore,
		router:     router,
		metrics:    met,
		prober:     prober,
		listener:   server.NewListener(ln, handler),
		metricsSrv: metricsSrv,
	}
	return d, ln.Addr().String()
}

// TestServeUntilShutdown_SignalCancellationDoesNotKillLiveConnections is
// the round-3 regression test for task 30.5: the previous coverage of this
// invariant (listener_signal_test.go, deleted in round 3) discarded the
// very ctx it claimed to cancel and could not fail under any mutation.
// Listener.Serve's signature now makes it structurally impossible to wire
// a signal ctx into it directly, but nothing above that layer proved
// serveUntilShutdown itself preserves the invariant end-to-end: ctx being
// cancelled (simulating SIGTERM) must only ever lead to
// listener.Shutdown(drainTimeout) — an existing, fully-authenticated
// connection must survive at least past that instant, not die with it.
func TestServeUntilShutdown_SignalCancellationDoesNotKillLiveConnections(t *testing.T) {
	backendCertPEM, backendKeyPEM := genCert(t, "backend.internal")
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (fake backend): %v", err)
	}
	defer backendLn.Close()
	runFakeBackend(t, backendLn, backendCertPEM, backendKeyPEM)

	cfgYAML := `
shutdown:
  drainTimeout: 3s
timeouts:
  handshake: 2s
  backendDial: 2s
  idle: 60s
`
	d, addr := buildTestDeps(t, cfgYAML, backendLn.Addr().String(), "tenant1.db.test", "tenant1")

	ctx, cancel := context.WithCancel(context.Background())
	logger := log.New(io.Discard, "", 0)
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveUntilShutdown(ctx, logger, d) }()

	// Dial a real client through the real Listener, all the way to a
	// successfully-authenticated connection — the exact state a live
	// query session would be in when SIGTERM arrives.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(sslRequestPacket()); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read SSLRequest reply: %v", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "tenant1.db.test", InsecureSkipVerify: true}) //nolint:gosec // test-only
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	if _, err := tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"})); err != nil {
		t.Fatalf("write StartupMessage: %v", err)
	}
	authOk := make([]byte, len(authOkBytes))
	if _, err := io.ReadFull(tlsConn, authOk); err != nil {
		t.Fatalf("read AuthenticationOk: %v", err)
	}

	// Drain the initial burst of heartbeat bytes so the read below observes
	// a *fresh* one, not one already buffered from before cancellation.
	drained := make([]byte, 1)
	tlsConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		if _, err := tlsConn.Read(drained); err != nil {
			break
		}
	}

	// Simulate SIGTERM: cancel the same ctx signal.NotifyContext would
	// produce in main.go.
	cancel()

	// The connection must keep receiving a steady stream of fresh
	// heartbeat bytes from the backend for a good while after
	// cancellation, not just one more already-in-flight byte: a single
	// Read succeeding is not proof of life, since a byte written by the
	// backend microseconds before an instant force-close can already be
	// sitting in the OS receive buffer and would be returned regardless.
	// Counting several arrivals across a window much longer than the
	// 15ms heartbeat interval is what actually distinguishes "still
	// relaying" from "torn down, but one stale byte survived."
	const wantHeartbeats = 5
	const readWindow = 300 * time.Millisecond
	got := make([]byte, 1)
	n := 0
	deadlineAt := time.Now().Add(readWindow)
	tlsConn.SetReadDeadline(deadlineAt)
	for time.Now().Before(deadlineAt) {
		if _, err := tlsConn.Read(got); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break // the read window elapsed with nothing more to read — expected, not a failure
			}
			t.Fatalf("connection was torn down by ctx cancellation alone after %d heartbeat(s) (want it to survive until drainTimeout or client close): %v", n, err)
		}
		n++
	}
	if n < wantHeartbeats {
		t.Fatalf("received only %d heartbeat byte(s) in %v after ctx cancellation, want at least %d — connection looks torn down early", n, readWindow, wantHeartbeats)
	}

	// Now close the client side cleanly so drain can complete quickly
	// rather than waiting out the full drainTimeout.
	tlsConn.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveUntilShutdown returned %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilShutdown did not return after the client closed and ctx was cancelled")
	}
}

// TestServeUntilShutdown_CancelLimitsFollowProxyProtocolGating is the
// paired round-3 regression test for task 30.5's other half: proving
// cancelLimitsFrom's topology gating (task 28.1/28.2) is actually wired
// into the real serving path buildDeps assembles, not just correct in
// isolation (already covered by cmd/pg-proxy's unit-level
// TestCancelLimitsFrom_* tests). With proxyProtocol disabled (the
// deployment default), every client here necessarily shares one source
// address (loopback), so a configured maxPerIP of 1 must NOT limit a
// second CancelRequest — only the independent, topology-agnostic
// maxInFlight ceiling may.
func TestServeUntilShutdown_CancelLimitsFollowProxyProtocolGating(t *testing.T) {
	var dialCount atomic.Int32
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (fake backend): %v", err)
	}
	defer backendLn.Close()
	go func() {
		for {
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			dialCount.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				header := make([]byte, 8)
				io.ReadFull(c, header)
				c.Write([]byte{'S'})
			}(conn)
		}
	}()

	cfgYAML := `
shutdown:
  drainTimeout: 2s
timeouts:
  handshake: 2s
  backendDial: 2s
  idle: 60s
limits:
  cancel:
    window: 1m
    maxPerIP: 1
    maxInFlight: 0
proxyProtocol:
  enabled: false
`
	d, addr := buildTestDeps(t, cfgYAML, backendLn.Addr().String(), "tenant1.db.test", "tenant1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.New(io.Discard, "", 0)
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveUntilShutdown(ctx, logger, d) }()

	sendOneCancel := func(t *testing.T) {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		conn.Write(sslRequestPacket())
		reply := make([]byte, 1)
		io.ReadFull(conn, reply)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: "tenant1.db.test", InsecureSkipVerify: true}) //nolint:gosec // test-only
		if err := tlsConn.Handshake(); err != nil {
			t.Fatalf("client TLS handshake: %v", err)
		}
		tlsConn.Write(cancelRequestPacket(1, 1))
		tlsConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		io.Copy(io.Discard, tlsConn)
	}

	sendOneCancel(t)
	sendOneCancel(t) // a real per-IP=1 limit would reject this; the topology gate must have disabled it

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dialCount.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := dialCount.Load(); got != 2 {
		t.Fatalf("backend dialed %d times, want exactly 2 — a per-IP cap of 1 was still being enforced even though proxyProtocol.enabled is false", got)
	}

	cancel()
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilShutdown did not return after ctx was cancelled and no live connections remained")
	}
}
