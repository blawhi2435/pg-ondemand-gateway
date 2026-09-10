package server

import (
	"crypto/rand"
	"encoding/hex"
)

// newConnID returns a short random identifier used to correlate a
// connection's connect/disconnect audit events (design §13's conn_id).
func newConnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read on the standard reader never errors
	return hex.EncodeToString(b)
}
