package server

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/blawhi2435/pg-ondemand-gateway/internal/audit"
	"github.com/blawhi2435/pg-ondemand-gateway/internal/metrics"
)

// TestAuditDisconnect_LogsAndCountsRejectedEvent is the round-2 regression
// test for discarded audit errors: audit.Sink.Disconnect returns an error
// specifically to enforce the design §13 reason domain, but the original
// call site did `_ = h.Audit.Disconnect(...)`. A rejected event vanished
// with no log line and no metric — silently defeating the one validation
// the audit package exists to perform. Simulates what would happen if a
// future Relay implementation (e.g. phase 2's message-parsing one) ever
// returned a reason internal/audit doesn't recognize.
func TestAuditDisconnect_LogsAndCountsRejectedEvent(t *testing.T) {
	var logBuf, auditBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	h := &Handler{Audit: audit.NewSink(&auditBuf), Metrics: metrics.New()}
	h.auditDisconnect("c1", "not_a_real_reason", 100, 200, 1.5)

	if auditBuf.Len() != 0 {
		t.Errorf("an invalid-reason disconnect event must not be written to the audit log, got:\n%s", auditBuf.String())
	}

	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_audit_errors_total", nil, 1) {
		t.Error("pgproxy_audit_errors_total was not incremented for a rejected audit event")
	}
	if !strings.Contains(logBuf.String(), "not_a_real_reason") {
		t.Errorf("rejected disconnect event was not logged:\n%s", logBuf.String())
	}
}

func TestAuditConnect_LogsAndCountsRejectedEvent(t *testing.T) {
	var logBuf, auditBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	h := &Handler{Audit: audit.NewSink(&auditBuf), Metrics: metrics.New()}
	h.auditConnect(audit.ConnectEvent{ConnID: "c1", Cluster: "tenant1", User: "", Database: "orders"})

	if auditBuf.Len() != 0 {
		t.Error("a connect event with an empty user must not be written to the audit log")
	}
	families, _ := h.Metrics.Registry().Gather()
	if !hasCounterSample(families, "pgproxy_audit_errors_total", nil, 1) {
		t.Error("pgproxy_audit_errors_total was not incremented for a rejected connect event")
	}
	if !strings.Contains(logBuf.String(), "c1") {
		t.Errorf("rejected connect event was not logged:\n%s", logBuf.String())
	}
}

func TestAuditDisconnect_ValidReasonWritesNormally(t *testing.T) {
	var logBuf, auditBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	h := &Handler{Audit: audit.NewSink(&auditBuf), Metrics: metrics.New()}
	h.auditDisconnect("c1", "client_close", 100, 200, 1.5)

	if !strings.Contains(auditBuf.String(), `"event":"disconnect"`) {
		t.Errorf("valid disconnect event was not written:\n%s", auditBuf.String())
	}
	families, _ := h.Metrics.Registry().Gather()
	if hasCounterSample(families, "pgproxy_audit_errors_total", nil, 1) {
		t.Error("pgproxy_audit_errors_total was incremented for a valid event")
	}
}
