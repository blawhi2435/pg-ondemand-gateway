package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// encodeHeader builds the 8-byte big-endian (length, code) header that every
// PostgreSQL first packet begins with.
func encodeHeader(length, code int32) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], uint32(length))
	binary.BigEndian.PutUint32(buf[4:8], uint32(code))
	return buf
}

func TestReadHeader_SSLRequest(t *testing.T) {
	r := bytes.NewReader(encodeHeader(8, sslRequestCode))

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Kind() != KindSSLRequest {
		t.Fatalf("Kind() = %v, want KindSSLRequest", h.Kind())
	}
}

func TestReadHeader_GSSENCRequest(t *testing.T) {
	r := bytes.NewReader(encodeHeader(8, gssEncRequestCode))

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Kind() != KindGSSENCRequest {
		t.Fatalf("Kind() = %v, want KindGSSENCRequest", h.Kind())
	}
}

func TestReadHeader_CancelRequest(t *testing.T) {
	r := bytes.NewReader(encodeHeader(16, cancelRequestCode))

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Kind() != KindCancelRequest {
		t.Fatalf("Kind() = %v, want KindCancelRequest", h.Kind())
	}
}

func TestReadHeader_PlaintextStartupMessage(t *testing.T) {
	r := bytes.NewReader(encodeHeader(41, protocolVersion3))

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Kind() != KindStartupMessage {
		t.Fatalf("Kind() = %v, want KindStartupMessage", h.Kind())
	}
	if h.Length != 41 {
		t.Fatalf("Length = %d, want 41", h.Length)
	}
}

func TestReadHeader_UnknownCode(t *testing.T) {
	r := bytes.NewReader(encodeHeader(8, 99999999))

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Kind() != KindUnknown {
		t.Fatalf("Kind() = %v, want KindUnknown for unrecognized code", h.Kind())
	}
}

// TestReadHeader_StartupMessage_RejectsOutOfRangeLength guards against the
// round-2 critical: Kind() previously classified KindStartupMessage on the
// code field alone, so a caller that sizes an allocation off Header.Length
// (as admitStartup does) could be handed a negative or multi-GB value
// straight off the wire, before ParseStartupMessage's own length check ever
// runs.
func TestReadHeader_StartupMessage_RejectsOutOfRangeLength(t *testing.T) {
	cases := []struct {
		name   string
		length int32
	}{
		{"zero", 0},
		{"belowHeaderSize", HeaderSize - 1},
		{"equalToHeaderSize", HeaderSize}, // no payload at all is not a valid StartupMessage
		{"negativeOnTheWire", -1},         // 0xFFFFFFFF
		{"maxInt32", 0x7FFFFFFF},
		{"oneOverMax", HeaderSize + MaxStartupMessageLength + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Header{Length: tc.length, Code: protocolVersion3}
			if h.Kind() == KindStartupMessage {
				t.Fatalf("Kind() = KindStartupMessage for out-of-range Length %d, want KindUnknown", tc.length)
			}
		})
	}
}

func TestReadHeader_StartupMessage_AcceptsInRangeLength(t *testing.T) {
	cases := []int32{HeaderSize + 1, 41, HeaderSize + MaxStartupMessageLength}
	for _, length := range cases {
		h := Header{Length: length, Code: protocolVersion3}
		if h.Kind() != KindStartupMessage {
			t.Errorf("Kind() for Length=%d = %v, want KindStartupMessage", length, h.Kind())
		}
	}
}

func TestReadHeader_ShortRead(t *testing.T) {
	// Only 3 bytes available before EOF: not enough for the 8-byte header.
	r := bytes.NewReader([]byte{0x00, 0x00, 0x00})

	_, err := ReadHeader(r)
	if err == nil {
		t.Fatal("ReadHeader: want error on short read, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadHeader error = %v, want an EOF-family error", err)
	}
}
