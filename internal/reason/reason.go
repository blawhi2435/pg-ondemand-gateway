// Package reason defines pg-proxy's disconnect-reason value domain (design
// §13), shared by internal/relay (which produces a subset of these as
// Run's own return value) and internal/audit (which validates every
// disconnect event against the full domain). Before this package existed,
// the two defined the same six strings independently — harmless as long as
// they happened to agree, but nothing would have caught them drifting
// apart if phase 2 added a reason to one and not the other.
package reason

const (
	ClientClose   = "client_close"
	BackendClose  = "backend_close"
	BackendError  = "backend_error"
	IdleTimeout   = "idle_timeout"
	Shutdown      = "shutdown"
	LimitExceeded = "limit_exceeded"
)
