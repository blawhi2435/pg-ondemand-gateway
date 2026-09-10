// Package registry implements seam C: the table of in-progress connections
// that backs connection limits, metrics, and (in phase 2) the revocation
// sweep (design §5, §9.3).
package registry

import (
	"errors"
	"net"
	"sync"
	"time"
)

// ErrTooManyConns is returned by TryAdmit/Add when a connection would
// exceed the global or per-cluster limit. Callers translate this into a
// SQLSTATE 53300 ErrorResponse.
var ErrTooManyConns = errors.New("registry: too many connections")

// Conn is one registered connection's full metadata. It's intentionally not
// just a counter: phase 2's revocation sweep will need to enumerate live
// connections and reach their sockets (via Snapshot, currently uncalled in
// phase 1). Phase 1's own graceful drain does NOT go through the Registry —
// Listener tracks and closes sockets itself (see Listener.conns) — so this
// type's clientConn/backendConn fields exist for phase 2, not for drain.
type Conn struct {
	ConnID          string
	Cluster         string
	SNI             string
	ClientIP        string
	User            string
	Database        string
	ApplicationName string
	Backend         string
	StartedAt       time.Time
	Revoked         bool // unused in phase 1; phase 2 sets this on revocation

	clientConn  net.Conn
	backendConn net.Conn
}

// ConnParams describes a connection to register. It's a named-field
// alternative to passing eight positional strings to NewConn — transposing
// any two of those compiled and passed every test while silently
// misattributing a connection's user or cluster in the audit log.
type ConnParams struct {
	ConnID          string
	Cluster         string
	SNI             string
	ClientIP        string
	User            string
	Database        string
	ApplicationName string
	Backend         string
	StartedAt       time.Time
	ClientConn      net.Conn
	BackendConn     net.Conn
}

// NewConn builds a Conn from p. ClientConn/BackendConn end up unexported on
// the result because nothing outside this package should close them
// directly today — phase 2's revocation sweep, which lives in this
// package, is the intended caller.
func NewConn(p ConnParams) *Conn {
	return &Conn{
		ConnID:          p.ConnID,
		Cluster:         p.Cluster,
		SNI:             p.SNI,
		ClientIP:        p.ClientIP,
		User:            p.User,
		Database:        p.Database,
		ApplicationName: p.ApplicationName,
		Backend:         p.Backend,
		StartedAt:       p.StartedAt,
		clientConn:      p.ClientConn,
		backendConn:     p.BackendConn,
	}
}

// Limits is the pair of caps TryAdmit/Add enforce, both per-pod (design
// §9.3). A zero value means "no limit" — used by tests that don't
// exercise limits.
type Limits struct {
	MaxConns           int
	MaxConnsPerCluster int
}

// LimitsFunc returns the currently-effective Limits. It's a function rather
// than a static value so callers can back it with a hot-reloadable config
// (internal/config) without the registry knowing anything about reload.
type LimitsFunc func() Limits

// Registry is seam C.
type Registry interface {
	// TryAdmit reserves a slot for cluster if doing so would stay within
	// both the global and per-cluster limits, and reports whether it
	// succeeded. The reservation counts toward both limits immediately —
	// this is what makes a pre-authentication connection (TLS done, backend
	// dialed, waiting on AuthenticationOk) count toward capacity, not just
	// the connections that have reached Add. A reservation is consumed by a
	// matching Add, or freed by a matching Release if the connection never
	// completes.
	TryAdmit(cluster string) bool
	// Release frees a reservation obtained via TryAdmit that will never be
	// completed with Add (e.g. the backend closed before authenticating).
	// Releasing more times than admitted for a cluster is a no-op.
	Release(cluster string)
	// Add registers a connection. If a prior TryAdmit for c.Cluster is
	// outstanding, Add consumes it unconditionally (the capacity was
	// already reserved). Otherwise Add re-checks the limits itself and
	// returns ErrTooManyConns if they'd be exceeded — a defensive path for
	// callers that don't go through TryAdmit (tests, mainly); production
	// code always calls TryAdmit first.
	Add(c *Conn) error
	// Remove unregisters a connection. Removing an unknown connID is a no-op.
	Remove(connID string)
	// Snapshot returns every currently registered connection.
	Snapshot() []*Conn
	// Count returns the total connection count and the count per cluster.
	// This reflects only committed (Add-ed) connections, not outstanding
	// TryAdmit reservations.
	Count() (total int, perCluster map[string]int)
}

// InMemoryRegistry is the phase 1 (and phase 2) Registry implementation:
// an in-memory map guarded by a mutex, with an incrementally maintained
// per-cluster count so Add/Remove stay O(1) regardless of connection count.
type InMemoryRegistry struct {
	mu           sync.Mutex
	conns        map[string]*Conn
	perCluster   map[string]int
	pending      map[string]int // outstanding TryAdmit reservations, per cluster
	pendingTotal int
	limits       LimitsFunc
}

// NewInMemoryRegistry returns an empty registry that enforces limits().
func NewInMemoryRegistry(limits LimitsFunc) *InMemoryRegistry {
	return &InMemoryRegistry{
		conns:      make(map[string]*Conn),
		perCluster: make(map[string]int),
		pending:    make(map[string]int),
		limits:     limits,
	}
}

// TryAdmit implements Registry.
func (r *InMemoryRegistry) TryAdmit(cluster string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	limits := r.limits()
	total := len(r.conns) + r.pendingTotal
	if limits.MaxConns > 0 && total >= limits.MaxConns {
		return false
	}
	perCluster := r.perCluster[cluster] + r.pending[cluster]
	if limits.MaxConnsPerCluster > 0 && perCluster >= limits.MaxConnsPerCluster {
		return false
	}

	r.pending[cluster]++
	r.pendingTotal++
	return true
}

// Release implements Registry.
func (r *InMemoryRegistry) Release(cluster string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseLocked(cluster)
}

// releaseLocked decrements a pending reservation for cluster. Caller must
// hold r.mu.
func (r *InMemoryRegistry) releaseLocked(cluster string) {
	if r.pending[cluster] <= 0 {
		return
	}
	r.pending[cluster]--
	r.pendingTotal--
	if r.pending[cluster] == 0 {
		delete(r.pending, cluster)
	}
}

// Add implements Registry.
func (r *InMemoryRegistry) Add(c *Conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pending[c.Cluster] > 0 {
		// Capacity was already reserved by TryAdmit; just consume it.
		r.releaseLocked(c.Cluster)
	} else {
		limits := r.limits()
		if limits.MaxConns > 0 && len(r.conns) >= limits.MaxConns {
			return ErrTooManyConns
		}
		if limits.MaxConnsPerCluster > 0 && r.perCluster[c.Cluster] >= limits.MaxConnsPerCluster {
			return ErrTooManyConns
		}
	}

	r.conns[c.ConnID] = c
	r.perCluster[c.Cluster]++
	return nil
}

// Remove implements Registry.
func (r *InMemoryRegistry) Remove(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.conns[connID]
	if !ok {
		return
	}
	delete(r.conns, connID)
	r.perCluster[c.Cluster]--
	if r.perCluster[c.Cluster] == 0 {
		delete(r.perCluster, c.Cluster)
	}
}

// Snapshot implements Registry.
func (r *InMemoryRegistry) Snapshot() []*Conn {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*Conn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

// Count implements Registry.
func (r *InMemoryRegistry) Count() (total int, perCluster map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	perCluster = make(map[string]int, len(r.perCluster))
	for cluster, count := range r.perCluster {
		perCluster[cluster] = count
	}
	return len(r.conns), perCluster
}
