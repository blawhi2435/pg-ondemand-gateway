// Package audit writes pg-proxy's connect/disconnect/cancel events as
// one-line JSON to stdout (design §13). This log is phase 1's most
// valuable artifact — phase 2's authorization reconciliation depends on
// every connect event carrying a non-empty user and database.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/reason"
)

// Disconnect reason value domain (design §13), aliasing the single shared
// source in internal/reason so relay and audit can never drift apart on
// these values. limit_exceeded and backend_error are audit-only — they
// describe outcomes decided before a Relay ever runs, unlike relay's own
// subset of these reasons.
const (
	ReasonClientClose   = reason.ClientClose
	ReasonBackendClose  = reason.BackendClose
	ReasonBackendError  = reason.BackendError
	ReasonIdleTimeout   = reason.IdleTimeout
	ReasonShutdown      = reason.Shutdown
	ReasonLimitExceeded = reason.LimitExceeded
)

var validReasons = map[string]bool{
	ReasonClientClose:   true,
	ReasonBackendClose:  true,
	ReasonBackendError:  true,
	ReasonIdleTimeout:   true,
	ReasonShutdown:      true,
	ReasonLimitExceeded: true,
}

// ConnectEvent carries every field the connect event schema requires.
type ConnectEvent struct {
	ConnID          string
	Cluster         string
	SNI             string
	ClientIP        string
	User            string
	Database        string
	ApplicationName string
	Backend         string
	TLS             string
	HandshakeMs     float64
}

// Sink writes audit events as one JSON object per line to w. Safe for
// concurrent use — one line per connection can be written from that
// connection's own goroutine.
type Sink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSink returns a Sink writing to w (typically os.Stdout).
func NewSink(w io.Writer) *Sink {
	return &Sink{w: w}
}

// Connect emits a connect event. It rejects the call (writing nothing) if
// User or Database is empty — every connect event MUST carry both.
func (s *Sink) Connect(e ConnectEvent) error {
	if e.User == "" {
		return fmt.Errorf("audit: connect event for conn_id %q has empty user", e.ConnID)
	}
	if e.Database == "" {
		return fmt.Errorf("audit: connect event for conn_id %q has empty database", e.ConnID)
	}

	return s.write(map[string]any{
		"ts":               timestamp(),
		"event":            "connect",
		"conn_id":          e.ConnID,
		"cluster":          e.Cluster,
		"sni":              e.SNI,
		"client_ip":        e.ClientIP,
		"user":             e.User,
		"database":         e.Database,
		"application_name": e.ApplicationName,
		"backend":          e.Backend,
		"tls":              e.TLS,
		"handshake_ms":     e.HandshakeMs,
	})
}

// Disconnect emits a disconnect event. disconnectReason must be one of the
// values in the design §13 value domain; anything else is rejected.
func (s *Sink) Disconnect(connID, disconnectReason string, bytesIn, bytesOut int64, durationS float64) error {
	if !validReasons[disconnectReason] {
		return fmt.Errorf("audit: disconnect event for conn_id %q has invalid reason %q", connID, disconnectReason)
	}

	return s.write(map[string]any{
		"ts":         timestamp(),
		"event":      "disconnect",
		"conn_id":    connID,
		"reason":     disconnectReason,
		"bytes_in":   bytesIn,
		"bytes_out":  bytesOut,
		"duration_s": durationS,
	})
}

// Cancel emits a cancel event for a forwarded CancelRequest. Per design
// §6.1, CancelRequest never produces connect/disconnect events — it isn't
// registered or authorized, just forwarded and logged. It is rate-limited
// (see CancelRejected for the event a rejection produces instead).
func (s *Sink) Cancel(cluster, sni, clientIP string) error {
	return s.write(map[string]any{
		"ts":        timestamp(),
		"event":     "cancel",
		"cluster":   cluster,
		"sni":       sni,
		"client_ip": clientIP,
	})
}

// CancelRejected emits an audit event for a CancelRequest that was refused
// before it reached the backend (e.g. the abuse-protection rate limit). A
// metric alone can only answer "how many"; incident response after a
// rate-limiting event needs "which client IP, when" — exactly what a
// silently-dropped rejection would have discarded.
func (s *Sink) CancelRejected(clientIP, rejectReason string) error {
	return s.write(map[string]any{
		"ts":        timestamp(),
		"event":     "cancel_rejected",
		"client_ip": clientIP,
		"reason":    rejectReason,
	})
}

func (s *Sink) write(event map[string]any) error {
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("audit: marshal event: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(line)
	return err
}

// timestamp returns the current time in the RFC3339Nano form used by every
// audit event's "ts" field.
func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
