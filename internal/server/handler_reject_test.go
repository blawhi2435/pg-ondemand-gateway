package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
)

func newTestHandler(t *testing.T, clientCert tls.Certificate, extra func(*Handler)) *Handler {
	t.Helper()
	h := &Handler{
		Router:     fakeRouter{},
		Authorizer: authzFunc(func(string, string, string, string) authz.Decision { return authz.Decision{Allow: true} }),
		Registry:   registry.NewInMemoryRegistry(fixedLimits(0, 0)),
		Relay:      relay.New(0),
		Audit:      audit.NewSink(&bytes.Buffer{}),
		Metrics:    metrics.New(),
		ClientTLS: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
		BackendTLS: &tls.Config{},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("dial should not be called in this test")
		},
		Timeouts: Timeouts{Handshake: 5 * time.Second, BackendDial: 2 * time.Second, Idle: 0},
	}
	if extra != nil {
		extra(h)
	}
	return h
}

// TestAdmitStartup_DefensiveLengthGuard is defense-in-depth for the round-2
// critical: even though pgwire.Header.Kind() now refuses to classify an
// out-of-range Length as KindStartupMessage (so HandleConn's loop can never
// reach admitStartup with one), admitStartup must not trust that invariant
// blindly — it lives in another package. A header fabricated directly
// (bypassing Kind()) must not reach the `make([]byte, ...)` call with a
// negative or oversized length.
func TestAdmitStartup_DefensiveLengthGuard(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")
	cases := []struct {
		name   string
		length int32
	}{
		{"belowHeaderSize", 0},
		{"negative", -1},
		{"hugeAllocation", 0x7FFFFFFF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientSide, proxyClientSide := net.Pipe()
			h := newTestHandler(t, clientCert, func(h *Handler) {
				h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("admitStartup panicked on Length=%d: %v", tc.length, r)
					}
				}()
				_, _, _, ok := h.admitStartup(proxyClientSide, pgwire.Header{Length: tc.length, Code: 196608}, "tenant1.db.test", "10.0.0.1")
				if ok {
					t.Errorf("admitStartup: ok = true for invalid Length=%d, want false", tc.length)
				}
			}()

			clientSide.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("admitStartup did not return")
			}
		})
	}
}

func TestHandleConn_UnknownSNI_RejectedExplicitly(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	h := newTestHandler(t, clientCert, nil) // empty router: no SNI known

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	if _, err := clientSide.Write(sslRequestPacket()); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	reply := make([]byte, 1)
	io.ReadFull(clientSide, reply)

	tlsConn := tls.Client(clientSide, &tls.Config{
		ServerName: "tenant1.db.test",
		RootCAs:    certPool(t, clientCADER),
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, tlsConn)
	if !bytes.Contains(errResp, []byte(pgwire.SQLStateConnectionFailure)) {
		t.Errorf("ErrorResponse missing SQLSTATE %s: %x", pgwire.SQLStateConnectionFailure, errResp)
	}
	drainAndClose(tlsConn)

	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "unknown_sni"}, 1) {
		t.Error("handshake_errors_total{reason=unknown_sni} was not incremented")
	}
}

func TestHandleConn_PlaintextStartup_Rejected(t *testing.T) {
	clientCert, _ := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	h := newTestHandler(t, clientCert, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleConn(context.Background(), proxyClientSide)
	}()

	// sslmode=disable: send a plaintext StartupMessage directly, no SSLRequest.
	// Written in the background: the handler rejects as soon as it reads the
	// 8-byte header, without draining the rest of the payload, and net.Pipe's
	// Write blocks until every byte is consumed — writing inline here would
	// deadlock against the Read below.
	go clientSide.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, clientSide)
	if !bytes.Contains(errResp, []byte("TLS")) {
		t.Errorf("ErrorResponse should mention TLS is required: %s", errResp)
	}
	clientSide.Close()

	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_handshake_errors_total", map[string]string{"reason": "plaintext_rejected"}, 1) {
		t.Error("handshake_errors_total{reason=plaintext_rejected} was not incremented")
	}
}

func TestHandleConn_BackendDialFailure_Returns08006(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		}
	})

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
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, tlsConn)
	if !bytes.Contains(errResp, []byte(pgwire.SQLStateConnectionFailure)) {
		t.Errorf("ErrorResponse missing SQLSTATE %s: %x", pgwire.SQLStateConnectionFailure, errResp)
	}
	drainAndClose(tlsConn)

	<-done

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_connections_total", map[string]string{"cluster": "tenant1", "result": "backend_error"}, 1) {
		t.Error("connections_total{cluster=tenant1,result=backend_error} was not incremented")
	}
}

func TestHandleConn_OverLimit_Returns53300WithoutDialing(t *testing.T) {
	clientCert, clientCADER := genTestCert(t, "tenant1.db.test")
	clientSide, proxyClientSide := net.Pipe()

	dialCalled := false
	h := newTestHandler(t, clientCert, func(h *Handler) {
		h.Router = fakeRouter{"tenant1.db.test": {Cluster: "tenant1", Addr: "backend.internal:5432"}}
		h.Registry = registry.NewInMemoryRegistry(func() registry.Limits { return registry.Limits{MaxConnsPerCluster: 1} })
		h.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("should not be called")
		}
	})
	// Pre-fill the registry so tenant1 is already at its per-cluster limit.
	h.Registry.Add(registry.NewConn(registry.ConnParams{
		ConnID: "existing", Cluster: "tenant1", SNI: "tenant1.db.test",
		ClientIP: "10.0.0.1", User: "u", Database: "d", Backend: "b", StartedAt: time.Now(),
	}))

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
	tlsConn.Write(buildStartupMessage(map[string]string{"user": "app_rw", "database": "orders"}))

	errResp := readErrorResponse(t, tlsConn)
	if !bytes.Contains(errResp, []byte(pgwire.SQLStateTooManyConnections)) {
		t.Errorf("ErrorResponse missing SQLSTATE %s: %x", pgwire.SQLStateTooManyConnections, errResp)
	}
	drainAndClose(tlsConn)

	<-done

	if dialCalled {
		t.Error("backend was dialed even though the connection was already over its per-cluster limit")
	}
}

// drainAndClose keeps reading (discarding) conn in the background so the
// peer's TLS Close (which writes a close_notify alert and blocks until it's
// read) doesn't stall on our own handshake deadline. The background read
// exits on its own once the peer actually closes.
func drainAndClose(conn net.Conn) {
	go io.Copy(io.Discard, conn)
}

// readErrorResponse reads exactly one ErrorResponse message ('E' + length +
// body) from r for assertions.
func readErrorResponse(t *testing.T, r io.Reader) []byte {
	t.Helper()
	typeByte := make([]byte, 1)
	if _, err := io.ReadFull(r, typeByte); err != nil {
		t.Fatalf("read ErrorResponse type byte: %v", err)
	}
	if typeByte[0] != 'E' {
		t.Fatalf("message type = %q, want 'E'", typeByte[0])
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		t.Fatalf("read ErrorResponse length: %v", err)
	}
	length := int(lenBuf[0])<<24 | int(lenBuf[1])<<16 | int(lenBuf[2])<<8 | int(lenBuf[3])
	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read ErrorResponse body: %v", err)
	}
	return append(typeByte, append(lenBuf, body...)...)
}

// hasCounterSample checks Gather() output for a counter/gauge sample
// matching name+labels with the given value.
func hasCounterSample(families []*dto.MetricFamily, name string, labels map[string]string, want float64) bool {
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if !labelsMatchExact(m.GetLabel(), labels) {
				continue
			}
			if m.Counter != nil && m.Counter.GetValue() == want {
				return true
			}
			if m.Gauge != nil && m.Gauge.GetValue() == want {
				return true
			}
		}
	}
	return false
}

func labelsMatchExact(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}
