package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/route"
)

// --- test fakes -------------------------------------------------------

type fakeRouter map[string]route.Route

func (f fakeRouter) Lookup(sni string) (route.Route, bool) {
	rt, ok := f[sni]
	return rt, ok
}

type authzFunc func(cluster, user, database, clientIP string) authz.Decision

func (f authzFunc) Check(cluster, user, database, clientIP string) authz.Decision {
	return f(cluster, user, database, clientIP)
}

// orderCheckingRegistry wraps a real registry, recording a violation if Add
// is ever called before authOkWritten is true.
type orderCheckingRegistry struct {
	*registry.InMemoryRegistry
	authOkWritten *atomic.Bool
	mu            sync.Mutex
	violations    []string
}

func (r *orderCheckingRegistry) Add(c *registry.Conn) error {
	if !r.authOkWritten.Load() {
		r.mu.Lock()
		r.violations = append(r.violations, "Registry.Add called before AuthenticationOk was written by the backend")
		r.mu.Unlock()
	}
	return r.InMemoryRegistry.Add(c)
}

func fixedLimits(maxConns, maxConnsPerCluster int) registry.LimitsFunc {
	return func() registry.Limits {
		return registry.Limits{MaxConns: maxConns, MaxConnsPerCluster: maxConnsPerCluster}
	}
}

// buildStartupMessage encodes a full StartupMessage packet (header + payload).
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

// splitConnectDisconnectLines finds and decodes the first connect and
// disconnect lines in an audit log buffer.
func splitConnectDisconnectLines(t *testing.T, logged string) (connect, disconnect map[string]any, ok bool) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode audit line %q: %v", line, err)
		}
		switch m["event"] {
		case "connect":
			connect = m
		case "disconnect":
			disconnect = m
		}
	}
	return connect, disconnect, connect != nil && disconnect != nil
}

func sslRequestPacket() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], 80877103)
	return buf
}

const authOkBytes = "\x52\x00\x00\x00\x08\x00\x00\x00\x00" // AuthenticationOk

// --- the big ordering test (16.1) --------------------------------------

func TestHandleConn_AuthorizerBeforeDial_RegistryAfterAuthOk(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	backendCert, backendCADER := genTestCert(t, "backend.internal")

	clientSide, proxyClientSide := net.Pipe()
	proxyBackendSide, backendSide := net.Pipe()

	var authorizerCalled atomic.Bool
	var orderViolations []string
	var orderMu sync.Mutex
	recordViolation := func(msg string) {
		orderMu.Lock()
		orderViolations = append(orderViolations, msg)
		orderMu.Unlock()
	}

	fakeDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		if !authorizerCalled.Load() {
			recordViolation("backend dialed before Authorizer.Check")
		}
		return proxyBackendSide, nil
	}

	authorizer := authzFunc(func(cluster, user, database, clientIP string) authz.Decision {
		authorizerCalled.Store(true)
		if cluster != "tenant1" || user != "app_rw" || database != "orders" {
			recordViolation("Authorizer.Check received unexpected arguments")
		}
		return authz.Decision{Allow: true}
	})

	var authOkWritten atomic.Bool
	reg := &orderCheckingRegistry{
		InMemoryRegistry: registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		authOkWritten:    &authOkWritten,
	}

	var auditBuf bytes.Buffer
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
		Dial:       fakeDialer,
		Timeouts:   Timeouts{Handshake: 5 * time.Second, BackendDial: 5 * time.Second, Idle: 0},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleConn(context.Background(), proxyClientSide)
	}()

	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		runFakeBackend(t, backendSide, backendCert, &authOkWritten)
	}()

	runFakeClient(t, clientSide, clientCADER)

	<-done
	<-backendDone

	orderMu.Lock()
	defer orderMu.Unlock()
	for _, v := range orderViolations {
		t.Error(v)
	}
	if !authorizerCalled.Load() {
		t.Error("Authorizer.Check was never called")
	}

	total, _ := reg.Count()
	if total != 0 {
		t.Errorf("registry not empty after connection ended: total=%d", total)
	}

	logged := auditBuf.String()
	connectLine, disconnectLine, ok := splitConnectDisconnectLines(t, logged)
	if !ok {
		t.Fatalf("audit log missing a connect and/or disconnect event:\n%s", logged)
	}
	// Values, not just line presence — this is what actually catches a
	// field-mapping bug across the handler -> audit boundary (e.g.
	// tlsVersionOf producing the wrong string, or cluster/user swapped).
	wantConnect := map[string]any{
		"cluster": "tenant1", "sni": "tenant1.db.test", "user": "app_rw",
		"database": "orders", "tls": "TLSv1.3",
	}
	for field, want := range wantConnect {
		if got := connectLine[field]; got != want {
			t.Errorf("connect event %s = %#v, want %#v", field, got, want)
		}
	}
	if hs, ok := connectLine["handshake_ms"].(float64); !ok || hs < 0 {
		t.Errorf("connect event handshake_ms = %#v, want a non-negative number", connectLine["handshake_ms"])
	}
	if disconnectLine["reason"] == "" {
		t.Error("disconnect event has an empty reason")
	}

	// pgproxy_connections_active must actually have been driven through a
	// real connection's Inc/Dec, not just asserted against a bare gauge —
	// this is the spec's own designated leak detector (Scenario「連線全部結
	// 束後歸零」).
	families, _ := handler.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_connections_active", map[string]string{"cluster": "tenant1"}, 0) {
		t.Error("pgproxy_connections_active did not return to 0 after a real connection ended")
	}

	// pgproxy_bytes_total must reflect the bytes Relay.Run actually
	// reported — it must not be a permanently-zero registered series.
	// The backend writes exactly authOkBytes+"PONG" backend->client, and
	// the client never sends anything once the relay phase starts.
	wantOut := float64(len(authOkBytes) + len("PONG"))
	if !hasCounterSample(families, "pgproxy_bytes_total", map[string]string{"cluster": "tenant1", "direction": "out"}, wantOut) {
		t.Errorf("pgproxy_bytes_total{cluster=tenant1,direction=out} did not reflect the relayed byte count (want %v)", wantOut)
	}
	if !hasCounterSample(families, "pgproxy_bytes_total", map[string]string{"cluster": "tenant1", "direction": "in"}, 0) {
		t.Error("pgproxy_bytes_total{cluster=tenant1,direction=in} series missing")
	}
}

// runFakeClient plays the psql side: SSLRequest, TLS handshake, StartupMessage,
// then reads until it sees AuthenticationOk and a follow-up marker, then closes.
func runFakeClient(t *testing.T, conn net.Conn, clientCADER []byte) {
	t.Helper()
	if _, err := conn.Write(sslRequestPacket()); err != nil {
		t.Fatalf("client: write SSLRequest: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("client: read SSLRequest reply: %v", err)
	}
	if reply[0] != 'S' {
		t.Fatalf("client: SSLRequest reply = %q, want 'S'", reply[0])
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client: TLS handshake: %v", err)
	}

	msg := buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"})
	if _, err := tlsConn.Write(msg); err != nil {
		t.Fatalf("client: write StartupMessage: %v", err)
	}

	buf := make([]byte, len(authOkBytes)+len("PONG"))
	if _, err := io.ReadFull(tlsConn, buf); err != nil {
		t.Fatalf("client: read AuthenticationOk+marker: %v", err)
	}
	if !bytes.Equal(buf[:len(authOkBytes)], []byte(authOkBytes)) {
		t.Fatalf("client: expected AuthenticationOk, got %x", buf[:len(authOkBytes)])
	}
	if string(buf[len(authOkBytes):]) != "PONG" {
		t.Fatalf("client: expected PONG marker, got %q", buf[len(authOkBytes):])
	}

	tlsConn.Close()
}

// runFakeBackend plays the Pooler side: accept the standard SSLRequest/TLS
// negotiation pg-proxy initiates, read the forwarded StartupMessage, then
// send AuthenticationOk followed by a data marker.
func runFakeBackend(t *testing.T, conn net.Conn, backendCert tls.Certificate, authOkWritten *atomic.Bool) {
	t.Helper()
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("backend: read SSLRequest: %v", err)
	}
	if _, err := conn.Write([]byte{'S'}); err != nil {
		t.Fatalf("backend: write SSLRequest reply: %v", err)
	}

	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{backendCert}})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("backend: TLS handshake: %v", err)
	}

	startupHeader, err := pgwire.ReadHeader(tlsConn)
	if err != nil {
		t.Fatalf("backend: read forwarded StartupMessage header: %v", err)
	}
	payload := make([]byte, int(startupHeader.Length)-pgwire.HeaderSize)
	if _, err := io.ReadFull(tlsConn, payload); err != nil {
		t.Fatalf("backend: read forwarded StartupMessage payload: %v", err)
	}
	sm, err := pgwire.ParseStartupMessage(payload)
	if err != nil {
		t.Fatalf("backend: forwarded StartupMessage did not parse: %v", err)
	}
	if sm.User != "app_rw" || sm.Database != "orders" {
		t.Fatalf("backend: forwarded StartupMessage = %+v, want user=app_rw database=orders", sm)
	}

	if _, err := tlsConn.Write([]byte(authOkBytes)); err != nil {
		t.Fatalf("backend: write AuthenticationOk: %v", err)
	}
	authOkWritten.Store(true)

	if _, err := tlsConn.Write([]byte("PONG")); err != nil {
		t.Fatalf("backend: write marker: %v", err)
	}

	// Wait for the client to close, which propagates as backend_close/
	// client_close and ends relay.Run on both sides.
	io.Copy(io.Discard, tlsConn)
}
