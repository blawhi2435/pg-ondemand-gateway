// Package server assembles the four seams (route, authz, registry, relay)
// into the actual per-connection flow (design §6) and owns graceful
// shutdown of the listener.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/authz"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/pgwire"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/proxyproto"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/registry"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/relay"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/route"
)

// Timeouts holds the three per-connection durations from design §8.1 that
// HandleConn needs directly (drainTimeout belongs to the listener instead).
type Timeouts struct {
	Handshake   time.Duration
	BackendDial time.Duration
	Idle        time.Duration
}

// Handler wires the four seams together for one connection. All fields are
// interfaces or plain config so tests can substitute fakes for every
// external dependency (k8s, real TLS certs, real backends).
type Handler struct {
	Router     route.Router
	Authorizer authz.Authorizer
	Registry   registry.Registry
	Relay      relay.Relay
	Audit      *audit.Sink
	Metrics    *metrics.Metrics

	ClientTLS  *tls.Config // GetCertificate + MinVersion set by the caller
	BackendTLS *tls.Config // RootCAs set by the caller; ServerName filled per-dial
	Dial       Dialer

	Timeouts Timeouts

	// ProxyProtocolEnabled and TrustedProxyNets implement design §9.4c.
	// When enabled, a connection's PROXY v1/v2 header is parsed for the
	// real client IP — but only from a peer address in TrustedProxyNets;
	// anything else is rejected outright rather than either trusting an
	// arbitrary peer's claimed IP (spoofable) or silently falling back to
	// the raw TCP peer address (which would look like the feature works
	// when it's actually being bypassed).
	ProxyProtocolEnabled bool
	TrustedProxyNets     []*net.IPNet

	// CancelLimiter enforces a per-source-IP rate limit and a global
	// in-flight cap on the CancelRequest path. CancelRequest is deliberately
	// exempt from Authorizer/Registry/connection limits (design §6.1) — but
	// that must not mean unlimited: each request costs a full backend TLS
	// dial with no other ceiling on it. nil disables rate limiting.
	CancelLimiter *CancelRateLimiter

	// NewConnID is overridable in tests for deterministic IDs; defaults to
	// newConnID (crypto/rand-based) when left nil by the caller.
	NewConnID func() string
}

// acceptProxyProtocol implements design §9.4c: parse a PROXY v1/v2 header
// before any pgwire byte is read, but only from a trusted peer. ok is false
// once the connection has been rejected and closed — an untrusted peer or
// an invalid header are both treated identically to "no valid first
// packet" (closed, no reply), never silently ignored.
func (h *Handler) acceptProxyProtocol(conn net.Conn, peerIP string) (realIP string, ok bool) {
	if !h.isTrustedProxySource(peerIP) {
		h.Metrics.IncHandshakeError(metrics.ReasonUntrustedProxySource)
		conn.Close()
		return "", false
	}
	result, err := proxyproto.Parse(conn)
	if err != nil {
		h.Metrics.IncHandshakeError(metrics.ReasonBadProxyHeader)
		conn.Close()
		return "", false
	}
	return result.ClientIP, true
}

// isTrustedProxySource reports whether peerIP is inside TrustedProxyNets.
// An empty TrustedProxyNets trusts nothing — enabling PROXY protocol
// without configuring an allowlist fails closed rather than either
// trusting every direct peer (spoofable) or silently behaving as if the
// feature were off.
func (h *Handler) isTrustedProxySource(peerIP string) bool {
	ip := net.ParseIP(peerIP)
	if ip == nil {
		return false
	}
	for _, n := range h.TrustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// upgradeToTLS answers a client's SSLRequest and performs the TLS
// handshake, returning the wrapped connection and the SNI it negotiated.
func (h *Handler) upgradeToTLS(ctx context.Context, current net.Conn) (net.Conn, string, bool) {
	if _, err := current.Write([]byte{'S'}); err != nil {
		current.Close()
		return nil, "", false
	}
	tlsConn := tls.Server(current, h.ClientTLS)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		h.Metrics.IncHandshakeError(metrics.ReasonTLSError)
		tlsConn.Close()
		return nil, "", false
	}
	return tlsConn, tlsConn.ConnectionState().ServerName, true
}

func (h *Handler) connID() string {
	if h.NewConnID != nil {
		return h.NewConnID()
	}
	return newConnID()
}

// HandleConn runs the full design §6 flow for one accepted connection. It
// always closes conn (directly, or via the TLS/relay layers) before
// returning.
func (h *Handler) HandleConn(ctx context.Context, conn net.Conn) {
	deadline := time.Now().Add(h.Timeouts.Handshake)
	_ = conn.SetDeadline(deadline)

	clientIP := hostOf(conn.RemoteAddr().String())
	current := conn

	if h.ProxyProtocolEnabled {
		realIP, ok := h.acceptProxyProtocol(current, clientIP)
		if !ok {
			return
		}
		if realIP != "" {
			clientIP = realIP
		}
	}

	h.dispatchFirstPacket(ctx, current, clientIP)
}

// dispatchFirstPacket implements design §6 steps 2-3: classify the first
// packet, looping on SSLRequest/GSSENCRequest (neither ends the
// connection), and hand off to the CancelRequest or StartupMessage path —
// the two cases that actually end this function.
func (h *Handler) dispatchFirstPacket(ctx context.Context, current net.Conn, clientIP string) {
	sni := ""
	haveTLS := false

	for {
		header, err := pgwire.ReadHeader(current)
		if err != nil {
			h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
			current.Close()
			return
		}

		switch header.Kind() {
		case pgwire.KindSSLRequest:
			next, negotiatedSNI, ok := h.upgradeToTLS(ctx, current)
			if !ok {
				return
			}
			current, sni, haveTLS = next, negotiatedSNI, true

		case pgwire.KindGSSENCRequest:
			if _, err := current.Write([]byte{'N'}); err != nil {
				current.Close()
				return
			}

		case pgwire.KindCancelRequest:
			h.handleCancel(current, header, sni, clientIP, haveTLS)
			return

		case pgwire.KindStartupMessage:
			h.handleStartup(ctx, current, header, sni, clientIP, haveTLS)
			return

		default:
			h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
			current.Close()
			return
		}
	}
}

// handleStartup implements design §6 steps 4-11 for a plaintext or
// post-TLS StartupMessage.
func (h *Handler) handleStartup(ctx context.Context, client net.Conn, header pgwire.Header, sni, clientIP string, haveTLS bool) {
	if !haveTLS {
		// sslmode=disable: proxy only accepts TLS (design §6.1).
		h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, "this proxy only accepts TLS connections")
		h.Metrics.IncHandshakeError(metrics.ReasonPlaintextRejected)
		return
	}

	rt, payload, sm, ok := h.admitStartup(client, header, sni, clientIP)
	if !ok {
		return
	}

	backend, ok := h.dialAndForwardStartup(ctx, client, header, payload, rt)
	if !ok {
		return
	}

	h.relayAfterAuth(ctx, client, backend, rt, sni, clientIP, sm)
}

// admitStartup runs design §6 steps 4-6: route lookup, StartupMessage
// parse, seam B authorization, and the early admission check. ok is false
// once the connection has already been rejected and closed.
func (h *Handler) admitStartup(client net.Conn, header pgwire.Header, sni, clientIP string) (rt route.Route, payload []byte, sm pgwire.StartupMessage, ok bool) {
	rt, payload, sm, ok = h.parseStartup(client, header, sni)
	if !ok {
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}
	if !h.authorizeAndReserve(client, rt, sm, clientIP) {
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}

	return rt, payload, sm, true
}

// parseStartup runs design §6 steps 4-5: route lookup and StartupMessage
// parse. ok is false once the connection has already been rejected and closed.
func (h *Handler) parseStartup(client net.Conn, header pgwire.Header, sni string) (rt route.Route, payload []byte, sm pgwire.StartupMessage, ok bool) {
	rt, found := h.Router.Lookup(sni)
	if !found {
		h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, fmt.Sprintf("unknown host %q", sni))
		h.Metrics.IncHandshakeError(metrics.ReasonUnknownSNI)
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}

	// Defense in depth: pgwire.Header.Kind() already refuses to classify an
	// out-of-range Length as KindStartupMessage, so HandleConn's dispatch
	// loop can't reach here with one — but that's an invariant living in
	// another package, and this allocation must not depend on it holding.
	if header.Length <= pgwire.HeaderSize || header.Length > pgwire.HeaderSize+pgwire.MaxStartupMessageLength {
		h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, "invalid startup message length")
		h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}

	payload = make([]byte, int(header.Length)-pgwire.HeaderSize)
	if _, err := io.ReadFull(client, payload); err != nil {
		h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
		client.Close()
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}
	sm, err := pgwire.ParseStartupMessage(payload)
	if err != nil {
		h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, "malformed startup message")
		h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
		return route.Route{}, nil, pgwire.StartupMessage{}, false
	}
	return rt, payload, sm, true
}

// authorizeAndReserve runs design §6 step 6 (seam B, which must run after
// parsing and before dialing the backend) plus the early admission check —
// distinct from the authoritative Registry.Add at step 9, this lets a
// connection that's already clearly over the limit fail fast with a clean
// 53300 instead of completing a whole backend dial and auth handshake
// first. It has a benign TOCTOU race with concurrent connections; the real
// enforcement is Add, which registerConnection still applies after
// AuthenticationOk.
func (h *Handler) authorizeAndReserve(client net.Conn, rt route.Route, sm pgwire.StartupMessage, clientIP string) bool {
	decision := h.Authorizer.Check(rt.Cluster, sm.User, sm.Database, clientIP)
	if !decision.Allow {
		h.rejectAndClose(client, decision.SQLState, decision.Reason)
		h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultDenied)
		return false
	}

	// Reserve capacity immediately — this is what makes a pre-auth
	// connection (TLS done, about to dial the backend, not yet
	// authenticated) count toward the limit, not just the connections that
	// reach registerConnection's Add. The reservation is consumed by Add on
	// success, or freed by Release on any failure between here and then
	// (dialAndForwardStartup's failure paths, and relayAfterAuth's
	// ctx-cancelled/backend-closed-without-auth paths).
	if !h.Registry.TryAdmit(rt.Cluster) {
		h.rejectAndClose(client, pgwire.SQLStateTooManyConnections, "too many connections")
		h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultLimitExceeded)
		return false
	}
	return true
}

// dialAndForwardStartup runs design §6 step 7: dial the backend and
// forward the StartupMessage bytes to it exactly as received. Every
// failure path here releases the TryAdmit reservation admitStartup took,
// since none of them reach registerConnection's Add.
func (h *Handler) dialAndForwardStartup(ctx context.Context, client net.Conn, header pgwire.Header, payload []byte, rt route.Route) (net.Conn, bool) {
	dialStart := time.Now()
	backend, err := dialBackend(ctx, h.dialer(), rt.Addr, h.BackendTLS, h.Timeouts.BackendDial)
	h.Metrics.ObserveBackendDialSeconds(rt.Cluster, time.Since(dialStart).Seconds())
	if err != nil {
		h.Registry.Release(rt.Cluster)
		h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, "backend unavailable")
		h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultBackendError)
		return nil, false
	}

	if _, err := backend.Write(encodeHeader(header)); err != nil {
		h.Registry.Release(rt.Cluster)
		h.failBackend(client, backend, rt.Cluster)
		return nil, false
	}
	if _, err := backend.Write(payload); err != nil {
		h.Registry.Release(rt.Cluster)
		h.failBackend(client, backend, rt.Cluster)
		return nil, false
	}
	return backend, true
}

// awaitAuthOutcome waits for whichever of AuthenticationOk, the backend
// closing, or ctx cancellation happens first, and registers the connection
// only if AuthenticationOk was actually observed. It reports whether
// registration succeeded.
//
// The three-way select has no inherent priority, and it doesn't need one:
// authOkWatcher.Read scans for AuthenticationOk *before* deciding whether
// to close Closed, so a backend that authenticates and then closes
// promptly (health checks, `psql -c`, any short session) can close both
// Detected and Closed from the very same Read. Go's select then picks
// between two ready cases uniformly at random — so instead of relying on
// which case fires, the Closed and ctx.Done() branches both re-check
// Detected themselves before falling back to Release. Whichever case wins
// the random pick, the outcome is the same: registered if Detected was
// ever closed, released otherwise.
func (h *Handler) awaitAuthOutcome(ctx context.Context, watched *authOkWatcher, connID string, rt route.Route, sni, clientIP string, sm pgwire.StartupMessage, client, backend net.Conn, started time.Time) bool {
	select {
	case <-watched.Detected:
		return h.registerConnection(connID, rt, sni, clientIP, sm, client, backend, started)
	case <-watched.Closed:
		// The realistic auth-failure case: the backend sent an
		// ErrorResponse (or just dropped the connection) instead of
		// AuthenticationOk. Without this case the goroutine would park on
		// Detected forever — HandleConn would never return, the connection
		// would never be untracked, and no disconnect event would ever be
		// written.
		return h.registerIfDetected(watched, connID, rt, sni, clientIP, sm, client, backend, started)
	case <-ctx.Done():
		return h.registerIfDetected(watched, connID, rt, sni, clientIP, sm, client, backend, started)
	}
}

// registerIfDetected non-blockingly re-checks Detected before releasing the
// reservation — see awaitAuthOutcome's comment for why this re-check, not
// select ordering, is what actually fixes the priority problem.
func (h *Handler) registerIfDetected(watched *authOkWatcher, connID string, rt route.Route, sni, clientIP string, sm pgwire.StartupMessage, client, backend net.Conn, started time.Time) bool {
	select {
	case <-watched.Detected:
		return h.registerConnection(connID, rt, sni, clientIP, sm, client, backend, started)
	default:
		h.Registry.Release(rt.Cluster)
		return false
	}
}

// relayAfterAuth watches the backend stream for AuthenticationOk (seam C
// must run only after that — design step 9), then runs the Relay for the
// rest of the connection's life and emits the disconnect event.
func (h *Handler) relayAfterAuth(ctx context.Context, client, backend net.Conn, rt route.Route, sni, clientIP string, sm pgwire.StartupMessage) {
	// Clear the handshake deadline set at the top of HandleConn before
	// handing the socket to the relay. SetDeadline there set both the read
	// and write deadlines; relay's own deadlineReader only ever refreshes
	// the read side, so a forgotten write deadline here would silently fail
	// every backend->client write once connect_time+Handshake passes,
	// however long the connection has actually been idle. dialBackend
	// already does the equivalent clear for the backend socket.
	_ = client.SetDeadline(time.Time{})
	watched := newAuthOkWatcher(backend)
	connID := h.connID()
	started := time.Now()

	var wasRegistered bool
	registered := make(chan struct{})
	go func() {
		defer close(registered)
		wasRegistered = h.awaitAuthOutcome(ctx, watched, connID, rt, sni, clientIP, sm, client, backend, started)
	}()

	bytesIn, bytesOut, reason := h.Relay.Run(ctx, client, watched)
	h.Metrics.AddBytes(rt.Cluster, metrics.DirectionIn, bytesIn)
	h.Metrics.AddBytes(rt.Cluster, metrics.DirectionOut, bytesOut)

	<-registered // Add/connect always happens-before Remove/disconnect below.
	if wasRegistered {
		h.Registry.Remove(connID)
		h.Metrics.DecActive(rt.Cluster)
	}
	h.auditDisconnect(connID, reason, bytesIn, bytesOut, time.Since(started).Seconds())
}

// registerConnection performs seam C's Add plus the connect audit event. It
// reports whether registration actually succeeded, so the caller knows
// whether a matching Remove/DecActive is owed later.
func (h *Handler) registerConnection(connID string, rt route.Route, sni, clientIP string, sm pgwire.StartupMessage, client, backend net.Conn, started time.Time) bool {
	c := registry.NewConn(registry.ConnParams{
		ConnID:          connID,
		Cluster:         rt.Cluster,
		SNI:             sni,
		ClientIP:        clientIP,
		User:            sm.User,
		Database:        sm.Database,
		ApplicationName: sm.ApplicationName,
		Backend:         rt.Addr,
		StartedAt:       started,
		ClientConn:      client,
		BackendConn:     backend,
	})
	if err := h.Registry.Add(c); err != nil {
		// admitStartup's TryAdmit already reserved this connection's slot,
		// so Add consumes that reservation unconditionally and this branch
		// should be unreachable in practice. It's kept as a defensive
		// fallback: AuthenticationOk has already reached the client by now,
		// so we can't safely send a 53300 ErrorResponse — Relay.Run's own
		// goroutines are concurrently reading/writing the same sockets, and
		// a stray write here would corrupt the stream. Closing both sides
		// is safe to do concurrently with Relay.Run (it's exactly how
		// Relay's own close-on-either-end works).
		client.Close()
		backend.Close()
		h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultLimitExceeded)
		return false
	}
	h.Metrics.IncActive(rt.Cluster)
	h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultOK)
	h.auditConnect(audit.ConnectEvent{
		ConnID:          connID,
		Cluster:         rt.Cluster,
		SNI:             sni,
		ClientIP:        clientIP,
		User:            sm.User,
		Database:        sm.Database,
		ApplicationName: sm.ApplicationName,
		Backend:         rt.Addr,
		TLS:             tlsVersionOf(client),
		HandshakeMs:     float64(time.Since(started).Microseconds()) / 1000.0,
	})
	return true
}

// auditConnect emits a connect event, logging and counting a rejection
// instead of silently discarding it. audit.Sink.Connect's error return
// exists specifically to enforce that user/database are never empty
// (design §13) — throwing that result away would mean a rejected event
// vanishes with no trace anywhere.
func (h *Handler) auditConnect(e audit.ConnectEvent) {
	if err := h.Audit.Connect(e); err != nil {
		log.Printf("server: audit connect event rejected: %v", err)
		h.Metrics.IncAuditError()
	}
}

// auditDisconnect emits a disconnect event, logging and counting a
// rejection instead of silently discarding it. audit.Sink.Disconnect's
// error return exists specifically to enforce the design §13 reason
// domain — if a future Relay ever produced a reason internal/audit doesn't
// recognize, this is what would surface that drift instead of the event
// just disappearing.
func (h *Handler) auditDisconnect(connID, disconnectReason string, bytesIn, bytesOut int64, durationS float64) {
	if err := h.Audit.Disconnect(connID, disconnectReason, bytesIn, bytesOut, durationS); err != nil {
		log.Printf("server: audit disconnect event rejected: %v", err)
		h.Metrics.IncAuditError()
	}
}

// auditCancel emits a cancel event, logging and counting a rejection
// instead of silently discarding it.
func (h *Handler) auditCancel(cluster, sni, clientIP string) {
	if err := h.Audit.Cancel(cluster, sni, clientIP); err != nil {
		log.Printf("server: audit cancel event rejected: %v", err)
		h.Metrics.IncAuditError()
	}
}

// auditCancelRejected emits a cancel_rejected event for a CancelRequest
// refused before it reached the backend. A metric increment alone can't
// answer which client IP was throttled or when — this keeps a rejected
// cancel from being silently discarded the way a bare counter would be.
func (h *Handler) auditCancelRejected(clientIP, rejectReason string) {
	if err := h.Audit.CancelRejected(clientIP, rejectReason); err != nil {
		log.Printf("server: audit cancel_rejected event rejected: %v", err)
		h.Metrics.IncAuditError()
	}
}

// failBackend closes both sides and reports a backend_error outcome after
// the backend accepted TLS but failed during the startup forward.
func (h *Handler) failBackend(client, backend net.Conn, cluster string) {
	h.rejectAndClose(client, pgwire.SQLStateConnectionFailure, "backend unavailable")
	backend.Close()
	h.Metrics.IncConnectionsTotal(cluster, metrics.ResultBackendError)
}

// rejectAndClose writes an ErrorResponse and closes the connection
// immediately, per design §6.3 ("write then close, never a silent RST").
func (h *Handler) rejectAndClose(conn net.Conn, sqlState, message string) {
	_, _ = conn.Write(pgwire.NewErrorResponse(sqlState, message))
	conn.Close()
}

func (h *Handler) dialer() Dialer {
	if h.Dial != nil {
		return h.Dial
	}
	return dialTCP
}

// tlsVersionOf reports the negotiated TLS version string for the audit
// log's "tls" field, or "" if conn isn't a *tls.Conn.
func tlsVersionOf(conn net.Conn) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	switch tlsConn.ConnectionState().Version {
	case tls.VersionTLS13:
		return "TLSv1.3"
	case tls.VersionTLS12:
		return "TLSv1.2"
	default:
		return "unknown"
	}
}

// cancelRequestBodyLen is the pid+secret payload that follows a
// CancelRequest's 8-byte header (design §6.1: length=16 total).
const cancelRequestBodyLen = 8

// handleCancel implements the CancelRequest path (design §6.1, §14): read
// the pid+secret, forward the whole packet to the routed backend over a
// standard-TLS connection, and emit only a cancel audit event — no
// registration, no Authorizer call, no connection limit.
func (h *Handler) handleCancel(client net.Conn, header pgwire.Header, sni, clientIP string, haveTLS bool) {
	defer client.Close()

	body := make([]byte, cancelRequestBodyLen)
	if _, err := io.ReadFull(client, body); err != nil {
		h.Metrics.IncHandshakeError(metrics.ReasonBadStartup)
		return
	}
	if !haveTLS {
		// No SNI available without TLS: nothing to route to. Design's
		// worked example assumes the cancel connection went through the
		// same SSLRequest+TLS dance as the original connection.
		h.Metrics.IncHandshakeError(metrics.ReasonUnknownSNI)
		return
	}

	rt, ok := h.Router.Lookup(sni)
	if !ok {
		h.Metrics.IncHandshakeError(metrics.ReasonUnknownSNI)
		return
	}

	if h.CancelLimiter != nil {
		if !h.CancelLimiter.Allow(clientIP) {
			h.Metrics.IncHandshakeError(metrics.ReasonCancelRateLimited)
			h.auditCancelRejected(clientIP, "cancel_rate_limited")
			return
		}
		defer h.CancelLimiter.Release()
	}

	backend, err := dialBackend(context.Background(), h.dialer(), rt.Addr, h.BackendTLS, h.Timeouts.BackendDial)
	if err != nil {
		h.Metrics.IncConnectionsTotal(rt.Cluster, metrics.ResultBackendError)
		return
	}
	defer backend.Close()

	packet := append(encodeHeader(header), body...)
	if _, err := backend.Write(packet); err != nil {
		return
	}
	h.auditCancel(rt.Cluster, sni, clientIP)
}
