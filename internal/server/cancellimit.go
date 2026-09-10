package server

import (
	"sync"
	"time"
)

// CancelLimits is the pair of caps CancelRateLimiter enforces. MaxPerIP <= 0
// disables the per-source-IP check (only MaxInFlight then applies) — this
// deployment's default topology routes every connection through APISIX,
// which shares one pod IP across all clients unless PROXY protocol is
// enabled and TrustedProxyNets recovers the real source, so a per-IP limit
// is only meaningful once that's true. MaxInFlight <= 0 disables the
// in-flight cap.
type CancelLimits struct {
	Window      time.Duration
	MaxPerIP    int
	MaxInFlight int
}

// CancelLimitsFunc returns the currently-effective CancelLimits. Like
// registry.LimitsFunc, it's a function rather than a static value so it can
// be backed by a hot-reloadable config.Store — every other bound in design
// §8.1 is hot-reloadable, and this one is no different now that it lives in
// the ConfigMap.
type CancelLimitsFunc func() CancelLimits

// CancelRateLimiter enforces a per-source-IP rate limit and a global
// in-flight cap on CancelRequest handling. Design deliberately exempts
// CancelRequest from Authorizer/Registry/connection limits — but "not
// registered" must not mean "not rate-limited": each request costs a full
// backend TLS dial (unmetered by maxConns/maxConnsPerCluster, so
// pgproxy_connections_active stays flat while file descriptors and
// PgBouncer's connection budget drain), and the forwarded 8-byte
// pid+secret is a 32-bit space that an unthrottled relay would otherwise
// let be brute-forced for free, per-attempt-unlogged.
type CancelRateLimiter struct {
	mu     sync.Mutex
	limits CancelLimitsFunc
	seen   map[string][]time.Time // recent request timestamps per source IP

	inFlight  int
	lastSweep time.Time
}

// NewCancelRateLimiter returns a limiter reading its bounds from limits().
func NewCancelRateLimiter(limits CancelLimitsFunc) *CancelRateLimiter {
	return &CancelRateLimiter{
		limits: limits,
		seen:   make(map[string][]time.Time),
	}
}

// Allow reports whether a new CancelRequest from clientIP may proceed. If
// it returns true, it has reserved an in-flight slot that the caller MUST
// free with exactly one matching Release call.
func (l *CancelRateLimiter) Allow(clientIP string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	limits := l.limits()
	now := time.Now()
	l.sweepLocked(now, limits.Window)

	kept := pruneBefore(l.seen[clientIP], now.Add(-limits.Window))
	perIPAllowed := limits.MaxPerIP <= 0 || len(kept) < limits.MaxPerIP
	inFlightAllowed := limits.MaxInFlight <= 0 || l.inFlight < limits.MaxInFlight
	allowed := perIPAllowed && inFlightAllowed

	if allowed {
		kept = append(kept, now)
		l.inFlight++
	}
	// An IP with no recent requests doesn't need an entry at all — this,
	// together with sweepLocked, is what keeps the map bounded to
	// currently-active source IPs rather than growing forever.
	if len(kept) == 0 {
		delete(l.seen, clientIP)
	} else {
		l.seen[clientIP] = kept
	}
	return allowed
}

// Release frees the in-flight slot a successful Allow reserved.
func (l *CancelRateLimiter) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
}

// sweepLocked evicts every source IP whose timestamps are all older than
// window, at most once per window — an amortized full-map walk rather than
// a per-call cost. Without this, an IP that appears once and never returns
// (trivially reachable: an IPv6 /64 has 2^64 usable source addresses, all
// legitimately reachable with a real handshake, not spoofing) keeps its
// entry forever, turning the anti-abuse map into its own memory-exhaustion
// vector. Caller must hold l.mu.
func (l *CancelRateLimiter) sweepLocked(now time.Time, window time.Duration) {
	if window <= 0 || now.Sub(l.lastSweep) < window {
		return
	}
	l.lastSweep = now
	cutoff := now.Add(-window)
	for ip, times := range l.seen {
		kept := pruneBefore(times, cutoff)
		if len(kept) == 0 {
			delete(l.seen, ip)
		} else {
			l.seen[ip] = kept
		}
	}
}

// seenCount reports the number of distinct source IPs currently tracked,
// for tests asserting the sweep bounds memory.
func (l *CancelRateLimiter) seenCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// pruneBefore returns the subset of times after cutoff, reusing times'
// backing array.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}
