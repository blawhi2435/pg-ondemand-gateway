package pgwire

import (
	"bytes"
	"errors"
	"fmt"
)

// MaxStartupMessageLength caps the accepted StartupMessage payload size.
// PostgreSQL's own limit is 10000 bytes (MAX_STARTUP_PACKET_LENGTH); pg-proxy
// uses the same value to reject malformed/oversized payloads before parsing.
const MaxStartupMessageLength = 10000

// ErrMissingUser is returned when a StartupMessage payload has no "user" key.
var ErrMissingUser = errors.New("pgwire: startup message missing required \"user\" parameter")

// ErrMalformedStartupMessage covers any structural problem with the
// StartupMessage payload: missing terminator, unpaired key/value, or a
// declared size over MaxStartupMessageLength.
var ErrMalformedStartupMessage = errors.New("pgwire: malformed startup message")

// StartupMessage holds the parameters pg-proxy cares about from a client's
// StartupMessage. Per design §5/§6.2 these are extracted but never used to
// make an allow/deny decision here — that's the Authorizer's job.
type StartupMessage struct {
	User            string
	Database        string
	ApplicationName string
}

// ParseStartupMessage parses the StartupMessage payload (everything after
// the 8-byte header): a sequence of NUL-terminated "key\0value\0" pairs,
// terminated by an extra NUL. It never panics on malformed input.
func ParseStartupMessage(payload []byte) (StartupMessage, error) {
	if len(payload) > MaxStartupMessageLength {
		return StartupMessage{}, fmt.Errorf("%w: payload of %d bytes exceeds limit of %d", ErrMalformedStartupMessage, len(payload), MaxStartupMessageLength)
	}
	// A well-formed payload is a run of "key\0value\0" pairs followed by one
	// extra NUL terminator, so it must end in two consecutive NUL bytes: the
	// last value's own terminator, then the payload terminator.
	if len(payload) < 2 || payload[len(payload)-1] != 0 || payload[len(payload)-2] != 0 {
		return StartupMessage{}, fmt.Errorf("%w: payload not double-NUL-terminated", ErrMalformedStartupMessage)
	}

	// Strip only the extra terminator; the last pair's own NUL stays so that
	// splitting still yields a trailing empty field to discard below.
	params, err := splitParams(payload[:len(payload)-1])
	if err != nil {
		return StartupMessage{}, err
	}

	user, ok := params["user"]
	if !ok || user == "" {
		return StartupMessage{}, ErrMissingUser
	}

	database, ok := params["database"]
	if !ok || database == "" {
		database = user // PostgreSQL convention: database defaults to user.
	}

	return StartupMessage{
		User:            user,
		Database:        database,
		ApplicationName: params["application_name"],
	}, nil
}

// splitParams splits a NUL-delimited byte slice into key/value pairs. body
// still carries the last pair's trailing NUL (only the payload's extra
// terminator has been stripped by the caller), so splitting always leaves
// one trailing empty field that isn't a real key or value.
func splitParams(body []byte) (map[string]string, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: no parameters", ErrMalformedStartupMessage)
	}

	fields := bytes.Split(body, []byte{0})
	fields = fields[:len(fields)-1] // drop the trailing empty artifact
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("%w: unpaired key/value (%d fields)", ErrMalformedStartupMessage, len(fields))
	}

	params := make(map[string]string, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key := string(fields[i])
		if key == "" {
			return nil, fmt.Errorf("%w: empty parameter key", ErrMalformedStartupMessage)
		}
		params[key] = string(fields[i+1])
	}
	return params, nil
}
