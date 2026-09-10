package pgwire

import (
	"bytes"
	"encoding/binary"
)

// SQLSTATE values pg-proxy needs to emit. 28000 and 57P01 are unused in
// phase 1 but the encoder must accept any SQLSTATE (design §14) so phase 2
// can use them without changing this package.
const (
	SQLStateConnectionFailure     = "08006" // backend unreachable / backend handshake failed
	SQLStateTooManyConnections    = "53300" // over the connection limit
	SQLStateInvalidAuthorization  = "28000" // phase 2: account/database not authorized
	SQLStateAdminShutdown         = "57P01" // phase 2: existing connection revoked
	errorResponseSeverity         = "FATAL"
	errorResponseLengthFieldBytes = 4 // the length field's own size, included in its own value
)

// NewErrorResponse builds a wire-format ErrorResponse ('E' message) carrying
// the given SQLSTATE and human-readable message, per design §6.3. Callers
// write the returned bytes to the client socket and then close the
// connection immediately — pg-proxy never waits for a response to it.
func NewErrorResponse(sqlState, message string) []byte {
	var body bytes.Buffer
	writeField(&body, 'S', errorResponseSeverity)
	writeField(&body, 'V', errorResponseSeverity)
	writeField(&body, 'C', sqlState)
	writeField(&body, 'M', message)
	body.WriteByte(0) // terminates the field list

	length := uint32(errorResponseLengthFieldBytes + body.Len())

	out := make([]byte, 0, 1+len(body.Bytes())+errorResponseLengthFieldBytes)
	out = append(out, 'E')
	lengthBuf := make([]byte, errorResponseLengthFieldBytes)
	binary.BigEndian.PutUint32(lengthBuf, length)
	out = append(out, lengthBuf...)
	out = append(out, body.Bytes()...)
	return out
}

// writeField appends one NUL-terminated ErrorResponse field: a one-byte
// field code followed by its NUL-terminated string value.
func writeField(buf *bytes.Buffer, code byte, value string) {
	buf.WriteByte(code)
	buf.WriteString(value)
	buf.WriteByte(0)
}
