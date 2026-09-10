// Package route implements seam A: the SNI → backend routing table built
// from CNPG Pooler CR annotations (design §5, §7).
package route

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// backendPort is the fixed port pg-proxy dials on every Pooler Service.
const backendPort = 5432

// Route is what a successful Lookup returns: the cluster identifier (used
// for audit logging and, in phase 2, the Redis policy key) and the backend
// address to dial.
type Route struct {
	Cluster string
	Addr    string
}

// Router is seam A. The read path (Lookup) is the hot path — one call per
// new connection — and must never block on a lock.
type Router interface {
	Lookup(sni string) (Route, bool)
}

// PoolerInfo is the subset of a Pooler CR that rebuild needs: identity plus
// the two pg-proxy annotations. Decoupling it from the k8s API types keeps
// rebuild/validation testable without a fake cluster.
type PoolerInfo struct {
	Namespace string
	Name      string
	Hostname  string // pg-proxy.internal/hostname annotation, empty if absent
	Cluster   string // pg-proxy.internal/cluster annotation
}

// EventRecorder reports validation problems found while rebuilding the
// table (invalid or duplicate hostnames), so they're visible as k8s Events
// rather than only in pg-proxy's own logs.
type EventRecorder interface {
	Warn(namespace, name, reason, message string)
}

const (
	reasonInvalidHostname   = "InvalidHostname"
	reasonDuplicateHostname = "DuplicateHostname"
	reasonMissingCluster    = "MissingCluster"
)

// table maps SNI hostname to Route. It is never mutated in place — Rebuild
// always constructs a new one and atomically swaps it in, so Lookup never
// needs a lock (design §7.3).
type table map[string]Route

// Table is the informer-independent half of seam A: it holds the current
// route table and knows how to validate and rebuild it from a Pooler list.
type Table struct {
	current atomic.Pointer[table]
}

// NewTable returns an empty, ready-to-use Table.
func NewTable() *Table {
	t := &Table{}
	empty := make(table)
	t.current.Store(&empty)
	return t
}

// Lookup is the read path: lock-free, safe to call concurrently with
// Rebuild from any number of goroutines. sni is matched case-insensitively
// (RFC 6066: TLS SNI is case-insensitive), against hostnames Rebuild
// already normalized to lowercase.
func (t *Table) Lookup(sni string) (Route, bool) {
	route, ok := (*t.current.Load())[strings.ToLower(sni)]
	return route, ok
}

// Len reports the number of entries in the current table, for the
// pgproxy_route_table_entries metric.
func (t *Table) Len() int {
	return len(*t.current.Load())
}

// Rebuild computes a brand-new table from the given Poolers and atomically
// swaps it in. Poolers without a hostname annotation are silently skipped.
// A multi-label hostname or a hostname collision is rejected and reported
// via events; the first Pooler in the slice that claims a hostname wins.
func (t *Table) Rebuild(poolers []PoolerInfo, events EventRecorder) {
	next := make(table, len(poolers))
	for _, p := range poolers {
		if p.Hostname == "" {
			continue
		}
		if err := validateSingleLabel(p.Hostname); err != nil {
			events.Warn(p.Namespace, p.Name, reasonInvalidHostname, err.Error())
			continue
		}
		if p.Cluster == "" {
			// An empty cluster identifier would otherwise flow straight into
			// the audit log's cluster field, every metric's cluster label,
			// and (phase 2) the Redis policy key — a typo in the annotation
			// must be visible, not a silently unattributable tenant.
			events.Warn(p.Namespace, p.Name, reasonMissingCluster,
				fmt.Sprintf("pooler has hostname %q but no cluster annotation", p.Hostname))
			continue
		}
		hostname := strings.ToLower(p.Hostname) // RFC 6066: SNI is case-insensitive
		if _, dup := next[hostname]; dup {
			events.Warn(p.Namespace, p.Name, reasonDuplicateHostname,
				fmt.Sprintf("hostname %q already claimed by another Pooler", p.Hostname))
			continue
		}
		next[hostname] = Route{
			Cluster: p.Cluster,
			Addr:    fmt.Sprintf("%s.%s.svc:%d", p.Name, p.Namespace, backendPort),
		}
	}
	t.current.Store(&next)
}

// wildcardLabelCount is the number of dot-separated labels a valid hostname
// must have: one tenant label plus the two-label apex domain the wildcard
// certificate covers (e.g. "tenant1.db.test" = tenant1 + db + test).
const wildcardLabelCount = 3

// validateSingleLabel rejects any hostname that inserts more than one label
// in front of the wildcard certificate's apex domain: the certificate
// (*.db.test) only covers one level, so "a.b.db.test" would fail TLS
// verification even though "tenant1.db.test" succeeds.
func validateSingleLabel(hostname string) error {
	labels := strings.Split(hostname, ".")
	if len(labels) != wildcardLabelCount {
		return fmt.Errorf("hostname %q must be a single label under the wildcard domain (e.g. tenant1.db.test)", hostname)
	}
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("hostname %q has an empty label", hostname)
		}
	}
	return nil
}
