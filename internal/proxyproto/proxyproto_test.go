package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestParse_V1_TextFormat(t *testing.T) {
	line := "PROXY TCP4 192.168.0.1 192.168.0.11 56324 5432\r\n"
	r := strings.NewReader(line)

	result, err := Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.Version != 1 {
		t.Errorf("Version = %d, want 1", result.Version)
	}
	if result.ClientIP != "192.168.0.1" {
		t.Errorf("ClientIP = %q, want 192.168.0.1", result.ClientIP)
	}
}

func TestParse_V1_UnknownProtocol(t *testing.T) {
	line := "PROXY UNKNOWN\r\n"
	r := strings.NewReader(line)

	result, err := Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.ClientIP != "" {
		t.Errorf("ClientIP = %q, want empty for UNKNOWN", result.ClientIP)
	}
}

// buildV2Header constructs a binary PROXY v2 header carrying an IPv4
// PROXY command with the given source address.
func buildV2Header(t *testing.T, srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("\r\n\r\n\x00\r\nQUIT\n")
	buf.WriteByte(0x21) // version 2, command PROXY
	buf.WriteByte(0x11) // AF_INET, STREAM
	addr := make([]byte, 12)
	copy(addr[0:4], srcIP.To4())
	copy(addr[4:8], dstIP.To4())
	binary.BigEndian.PutUint16(addr[8:10], srcPort)
	binary.BigEndian.PutUint16(addr[10:12], dstPort)
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(addr)))
	buf.Write(lenBuf)
	buf.Write(addr)
	return buf.Bytes()
}

func TestParse_V2_BinaryFormat(t *testing.T) {
	header := buildV2Header(t, net.ParseIP("10.0.0.5"), 54321, net.ParseIP("10.0.0.100"), 5432)
	r := bytes.NewReader(header)

	result, err := Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.Version != 2 {
		t.Errorf("Version = %d, want 2", result.Version)
	}
	if result.ClientIP != "10.0.0.5" {
		t.Errorf("ClientIP = %q, want 10.0.0.5", result.ClientIP)
	}
}

func TestParse_V2_LocalCommand(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("\r\n\r\n\x00\r\nQUIT\n")
	buf.WriteByte(0x20) // version 2, command LOCAL
	buf.WriteByte(0x00)
	buf.Write([]byte{0x00, 0x00}) // zero-length address block
	r := bytes.NewReader(buf.Bytes())

	result, err := Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.ClientIP != "" {
		t.Errorf("ClientIP = %q, want empty for LOCAL command", result.ClientIP)
	}
}

func TestParse_RejectsNonPROXYFirstBytes(t *testing.T) {
	// What a plain SSLRequest first packet looks like: definitely not a
	// PROXY header. When proxyProtocol.enabled=true, this must be rejected
	// rather than silently treated as absent.
	garbage := bytes.Repeat([]byte{0x00}, 16)
	r := bytes.NewReader(garbage)

	_, err := Parse(r)
	if !errors.Is(err, ErrInvalidHeader) {
		t.Fatalf("Parse error = %v, want ErrInvalidHeader", err)
	}
}

func TestParse_RejectsTruncatedInput(t *testing.T) {
	r := bytes.NewReader([]byte("PROX"))

	_, err := Parse(r)
	if !errors.Is(err, ErrInvalidHeader) {
		t.Fatalf("Parse error = %v, want ErrInvalidHeader", err)
	}
}
