package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func validConnectEvent() ConnectEvent {
	return ConnectEvent{
		ConnID:          "01K5R7Q2XJ",
		Cluster:         "tenant1",
		SNI:             "tenant1.db.test",
		ClientIP:        "10.42.7.19",
		User:            "analyst_ro",
		Database:        "orders",
		ApplicationName: "metabase",
		Backend:         "tenant1-pooler.pgproxy-e2e.svc:5432",
		TLS:             "TLSv1.3",
		HandshakeMs:     3.1,
	}
}

func TestSink_Connect_WritesOneJSONLineWithRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	if err := sink.Connect(validConnectEvent()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]

	if _, ok := got["ts"]; !ok {
		t.Error("connect event missing field \"ts\"")
	}
	// Values, not just presence — a field-mapping bug (e.g. user and
	// database swapped) would pass a presence-only check but corrupt
	// exactly the artifact phase 2 reconciles against.
	want := map[string]any{
		"event": "connect", "conn_id": "01K5R7Q2XJ", "cluster": "tenant1",
		"sni": "tenant1.db.test", "client_ip": "10.42.7.19", "user": "analyst_ro",
		"database": "orders", "application_name": "metabase",
		"backend": "tenant1-pooler.pgproxy-e2e.svc:5432", "tls": "TLSv1.3", "handshake_ms": 3.1,
	}
	for field, wantVal := range want {
		if got[field] != wantVal {
			t.Errorf("%s = %#v, want %#v", field, got[field], wantVal)
		}
	}
}

func TestSink_Connect_RejectsEmptyUser(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	e := validConnectEvent()
	e.User = ""

	if err := sink.Connect(e); err == nil {
		t.Fatal("Connect: want error for empty user, got nil")
	}
	if buf.Len() != 0 {
		t.Error("Connect must not write an event when validation fails")
	}
}

func TestSink_Connect_RejectsEmptyDatabase(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	e := validConnectEvent()
	e.Database = ""

	if err := sink.Connect(e); err == nil {
		t.Fatal("Connect: want error for empty database, got nil")
	}
}

func TestSink_Disconnect_WritesRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	if err := sink.Disconnect("01K5R7Q2XJ", ReasonClientClose, 18422, 9931204, 375.8); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	lines := decodeLines(t, &buf)
	got := lines[0]
	if _, ok := got["ts"]; !ok {
		t.Error("disconnect event missing field \"ts\"")
	}
	want := map[string]any{
		"event": "disconnect", "conn_id": "01K5R7Q2XJ", "reason": ReasonClientClose,
		"bytes_in": float64(18422), "bytes_out": float64(9931204), "duration_s": 375.8,
	}
	for field, wantVal := range want {
		if got[field] != wantVal {
			t.Errorf("%s = %#v, want %#v", field, got[field], wantVal)
		}
	}
}

func TestSink_Disconnect_RejectsReasonOutsideDomain(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	if err := sink.Disconnect("c1", "made_up_reason", 0, 0, 0); err == nil {
		t.Fatal("Disconnect: want error for a reason outside the allowed domain, got nil")
	}
}

func TestSink_Disconnect_AllValidReasonsAccepted(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	for _, reason := range []string{
		ReasonClientClose, ReasonBackendClose, ReasonBackendError,
		ReasonIdleTimeout, ReasonShutdown, ReasonLimitExceeded,
	} {
		if err := sink.Disconnect("c1", reason, 0, 0, 0); err != nil {
			t.Errorf("Disconnect(reason=%q): %v", reason, err)
		}
	}
}

func TestSink_Cancel_WritesEventWithoutConnectOrDisconnect(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	if err := sink.Cancel("tenant1", "tenant1.db.test", "10.42.7.19"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]
	want := map[string]any{
		"event": "cancel", "cluster": "tenant1", "sni": "tenant1.db.test", "client_ip": "10.42.7.19",
	}
	for field, wantVal := range want {
		if got[field] != wantVal {
			t.Errorf("%s = %#v, want %#v", field, got[field], wantVal)
		}
	}
}

// TestSink_CancelRejected_WritesEventWithReason is the round-3 regression
// test for a rejected CancelRequest getting a metric with no audit trail —
// see server.Handler.auditCancelRejected's call site.
func TestSink_CancelRejected_WritesEventWithReason(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)

	if err := sink.CancelRejected("10.42.7.19", "cancel_rate_limited"); err != nil {
		t.Fatalf("CancelRejected: %v", err)
	}

	lines := decodeLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]
	want := map[string]any{
		"event": "cancel_rejected", "client_ip": "10.42.7.19", "reason": "cancel_rate_limited",
	}
	for field, wantVal := range want {
		if got[field] != wantVal {
			t.Errorf("%s = %#v, want %#v", field, got[field], wantVal)
		}
	}
	if _, ok := got["ts"]; !ok {
		t.Error("cancel_rejected event missing field \"ts\"")
	}
}
