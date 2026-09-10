// Package pgwire implements the slice of the PostgreSQL wire protocol that
// pg-proxy needs to terminate TLS and route connections: first-packet
// classification, StartupMessage parsing, and ErrorResponse generation.
package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MessageKind identifies which of the four first-packet shapes a connection
// sent (see design §6.1).
type MessageKind int

const (
	// KindUnknown covers any code pg-proxy doesn't recognize.
	KindUnknown MessageKind = iota
	KindSSLRequest
	KindGSSENCRequest
	KindCancelRequest
	KindStartupMessage
)

// Wire-protocol magic numbers and fixed lengths for the four first-packet
// shapes (design §6.1).
const (
	sslRequestCode    int32 = 80877103
	gssEncRequestCode int32 = 80877104
	cancelRequestCode int32 = 80877102
	protocolVersion3  int32 = 196608

	fixedRequestLength  int32 = 8  // SSLRequest / GSSENCRequest: length includes itself + code
	cancelRequestLength int32 = 16 // CancelRequest: length + code + pid + secret
)

// HeaderSize is the number of bytes every first packet begins with: two
// big-endian int32 fields, length and code.
const HeaderSize = 8

// Header is the first 8 bytes of a connection: the declared length and the
// protocol code that follows it.
type Header struct {
	Length int32
	Code   int32
}

// Kind classifies the header into one of the four known first-packet shapes,
// or KindUnknown if the (length, code) pair doesn't match any of them.
func (h Header) Kind() MessageKind {
	switch {
	case h.Length == fixedRequestLength && h.Code == sslRequestCode:
		return KindSSLRequest
	case h.Length == fixedRequestLength && h.Code == gssEncRequestCode:
		return KindGSSENCRequest
	case h.Length == cancelRequestLength && h.Code == cancelRequestCode:
		return KindCancelRequest
	case h.Code == protocolVersion3 &&
		h.Length > HeaderSize &&
		h.Length <= HeaderSize+MaxStartupMessageLength:
		return KindStartupMessage
	default:
		return KindUnknown
	}
}

// ReadHeader reads the fixed 8-byte header from r. Callers are expected to
// have already installed a handshake deadline on the underlying connection;
// a short read surfaces as an EOF-family error.
func ReadHeader(r io.Reader) (Header, error) {
	var buf [HeaderSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return Header{}, fmt.Errorf("pgwire: read first-packet header: %w", err)
	}
	return Header{
		Length: int32(binary.BigEndian.Uint32(buf[0:4])),
		Code:   int32(binary.BigEndian.Uint32(buf[4:8])),
	}, nil
}
