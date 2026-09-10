package pgwire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildExpectedErrorResponse assembles the wire bytes by hand, independent
// of the production encoder, so the test catches a wrong-by-construction
// implementation rather than mirroring it.
func buildExpectedErrorResponse(sqlState, message string) []byte {
	var body bytes.Buffer
	body.WriteByte('S')
	body.WriteString("FATAL")
	body.WriteByte(0)
	body.WriteByte('V')
	body.WriteString("FATAL")
	body.WriteByte(0)
	body.WriteByte('C')
	body.WriteString(sqlState)
	body.WriteByte(0)
	body.WriteByte('M')
	body.WriteString(message)
	body.WriteByte(0)
	body.WriteByte(0) // field terminator

	length := int32(4 + body.Len()) // length field includes itself, not the 'E' type byte

	var out bytes.Buffer
	out.WriteByte('E')
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(length))
	out.Write(lenBuf)
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestNewErrorResponse_ByteFormat(t *testing.T) {
	got := NewErrorResponse(SQLStateConnectionFailure, "backend unavailable")
	want := buildExpectedErrorResponse(SQLStateConnectionFailure, "backend unavailable")

	if !bytes.Equal(got, want) {
		t.Fatalf("NewErrorResponse bytes mismatch:\n got: %x\nwant: %x", got, want)
	}
}

func TestNewErrorResponse_LengthFieldExcludesTypeByte(t *testing.T) {
	got := NewErrorResponse(SQLStateConnectionFailure, "x")

	if got[0] != 'E' {
		t.Fatalf("first byte = %q, want 'E'", got[0])
	}
	length := binary.BigEndian.Uint32(got[1:5])
	// Total bytes minus the 1 type byte must equal the declared length.
	if int(length) != len(got)-1 {
		t.Fatalf("length field = %d, want %d (total %d bytes minus 1 type byte)", length, len(got)-1, len(got))
	}
}

func TestNewErrorResponse_SQLStateBackendUnavailable(t *testing.T) {
	got := NewErrorResponse(SQLStateConnectionFailure, "dial backend")
	if !bytes.Contains(got, []byte("C"+SQLStateConnectionFailure+"\x00")) {
		t.Fatalf("ErrorResponse does not contain SQLSTATE field %q", SQLStateConnectionFailure)
	}
	if SQLStateConnectionFailure != "08006" {
		t.Fatalf("SQLStateConnectionFailure = %q, want 08006", SQLStateConnectionFailure)
	}
}

func TestNewErrorResponse_SQLStateTooManyConnections(t *testing.T) {
	if SQLStateTooManyConnections != "53300" {
		t.Fatalf("SQLStateTooManyConnections = %q, want 53300", SQLStateTooManyConnections)
	}
	got := NewErrorResponse(SQLStateTooManyConnections, "too many connections")
	if !bytes.Contains(got, []byte("C"+SQLStateTooManyConnections+"\x00")) {
		t.Fatalf("ErrorResponse does not contain SQLSTATE field %q", SQLStateTooManyConnections)
	}
}

func TestNewErrorResponse_ArbitrarySQLState(t *testing.T) {
	// Phase 2 needs 28000 (invalid_authorization_specification) and 57P01
	// (admin_shutdown / used for revocation); the encoder must not hardcode
	// a fixed set of states.
	for _, state := range []string{"28000", "57P01"} {
		got := NewErrorResponse(state, "phase 2 reason")
		if !bytes.Contains(got, []byte("C"+state+"\x00")) {
			t.Errorf("ErrorResponse for %q missing SQLSTATE field", state)
		}
	}
}
