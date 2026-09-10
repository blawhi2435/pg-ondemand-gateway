// Package proxyproto parses the PROXY protocol (v1 text and v2 binary),
// used — when explicitly enabled — to recover the real client IP when
// pg-proxy sits behind a load balancer that rewrites the source address
// (design §9.4c). Disabled by default; when enabled, a connection whose
// first bytes aren't a valid PROXY header is rejected outright rather than
// silently treated as if the option were off.
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// ErrInvalidHeader is returned for anything that isn't a well-formed v1 or
// v2 PROXY header.
var ErrInvalidHeader = errors.New("proxyproto: invalid PROXY protocol header")

// v2Signature is the fixed 12-byte magic every v2 header starts with.
var v2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	v1Prefix       = "PROXY "
	v1MaxLength    = 107 // per the PROXY protocol v1 spec, header including CRLF
	headerPeekSize = 12  // long enough to hold either the v1 prefix or the full v2 signature

	v2CommandLocal = 0x0
	v2FamilyINET   = 0x1
	v2FamilyINET6  = 0x2

	v2AddrLenINET  = 12 // 4+4 addresses + 2+2 ports
	v2AddrLenINET6 = 36 // 16+16 addresses + 2+2 ports
)

// Result is what Parse extracts: the real client IP (empty for the LOCAL
// v2 command or an UNKNOWN v1 connection, both of which are valid headers
// carrying no usable address) and which protocol version was seen.
type Result struct {
	ClientIP string
	Version  int
}

// Parse reads a PROXY protocol header from r and returns the client IP it
// declares. It consumes exactly the header's bytes and nothing more, so
// the pgwire first-packet bytes that follow are left untouched on r.
func Parse(r io.Reader) (Result, error) {
	prefix := make([]byte, headerPeekSize)
	if _, err := io.ReadFull(r, prefix); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidHeader, err)
	}

	if bytes.Equal(prefix, v2Signature) {
		return parseV2(r)
	}
	if string(prefix[:len(v1Prefix)]) == v1Prefix {
		return parseV1(r, prefix)
	}
	return Result{}, ErrInvalidHeader
}

// parseV2 parses the binary header that follows the 12-byte signature
// already consumed by Parse: a 4-byte fixed header, then a length-prefixed
// address block.
func parseV2(r io.Reader) (Result, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return Result{}, fmt.Errorf("%w: read v2 header: %v", ErrInvalidHeader, err)
	}
	verCmd, famProto := header[0], header[1]
	length := binary.BigEndian.Uint16(header[2:4])

	addr := make([]byte, length)
	if _, err := io.ReadFull(r, addr); err != nil {
		return Result{}, fmt.Errorf("%w: read v2 address block: %v", ErrInvalidHeader, err)
	}

	if version := verCmd >> 4; version != 2 {
		return Result{}, fmt.Errorf("%w: unsupported v2 version %d", ErrInvalidHeader, version)
	}
	if command := verCmd & 0x0F; command == v2CommandLocal {
		return Result{Version: 2}, nil
	}

	switch family := famProto >> 4; family {
	case v2FamilyINET:
		if len(addr) < v2AddrLenINET {
			return Result{}, fmt.Errorf("%w: v2 AF_INET address block too short", ErrInvalidHeader)
		}
		return Result{ClientIP: net.IP(addr[0:4]).String(), Version: 2}, nil
	case v2FamilyINET6:
		if len(addr) < v2AddrLenINET6 {
			return Result{}, fmt.Errorf("%w: v2 AF_INET6 address block too short", ErrInvalidHeader)
		}
		return Result{ClientIP: net.IP(addr[0:16]).String(), Version: 2}, nil
	default:
		return Result{}, fmt.Errorf("%w: unsupported v2 address family %d", ErrInvalidHeader, family)
	}
}

// parseV1 reassembles the text header line starting from the bytes Parse
// already peeked, reading one byte at a time until the terminating CRLF,
// bounded by the protocol's own maximum line length.
func parseV1(r io.Reader, prefix []byte) (Result, error) {
	line := append([]byte{}, prefix...)
	one := make([]byte, 1)
	for !bytes.HasSuffix(line, []byte("\r\n")) {
		if len(line) > v1MaxLength {
			return Result{}, fmt.Errorf("%w: v1 header exceeds maximum length", ErrInvalidHeader)
		}
		if _, err := io.ReadFull(r, one); err != nil {
			return Result{}, fmt.Errorf("%w: read v1 header: %v", ErrInvalidHeader, err)
		}
		line = append(line, one[0])
	}

	fields := strings.Fields(strings.TrimSuffix(string(line), "\r\n"))
	if len(fields) < 2 {
		return Result{}, fmt.Errorf("%w: v1 header has too few fields", ErrInvalidHeader)
	}
	if fields[1] == "UNKNOWN" {
		return Result{Version: 1}, nil
	}
	if len(fields) < 6 {
		return Result{}, fmt.Errorf("%w: v1 header missing address fields", ErrInvalidHeader)
	}
	return Result{ClientIP: fields[2], Version: 1}, nil
}
