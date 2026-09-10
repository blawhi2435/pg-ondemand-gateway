package server

import (
	"fmt"
	"testing"
	"time"
)

func fixedCancelLimits(window time.Duration, maxPerIP, maxInFlight int) CancelLimitsFunc {
	return func() CancelLimits {
		return CancelLimits{Window: window, MaxPerIP: maxPerIP, MaxInFlight: maxInFlight}
	}
}

func TestCancelRateLimiter_PerIPCap(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(time.Minute, 3, 0))

	for i := 0; i < 3; i++ {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("Allow: request %d under the per-IP cap was rejected", i)
		}
		l.Release()
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("Allow: 4th request within the window was accepted, want rejected")
	}
}

func TestCancelRateLimiter_DifferentIPsIndependent(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(time.Minute, 1, 0))

	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: first request for 10.0.0.1 was rejected")
	}
	if !l.Allow("10.0.0.2") {
		t.Fatal("Allow: a different source IP must not be throttled by another IP's usage")
	}
}

func TestCancelRateLimiter_WindowExpires(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(20*time.Millisecond, 1, 0))

	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: first request was rejected")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("Allow: second request within the window was accepted")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: request after the window expired was rejected")
	}
}

// TestCancelRateLimiter_GlobalInFlightCap is the "in-flight" half: cancel
// dials must be capped independently of the per-IP rate, so they can't
// consume the whole fd/connection budget even spread across many source IPs.
func TestCancelRateLimiter_GlobalInFlightCap(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(time.Minute, 100, 2))

	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: 1st in-flight slot rejected")
	}
	if !l.Allow("10.0.0.2") {
		t.Fatal("Allow: 2nd in-flight slot rejected")
	}
	if l.Allow("10.0.0.3") {
		t.Fatal("Allow: 3rd concurrent request accepted past the global in-flight cap")
	}

	l.Release() // free one of the first two
	if !l.Allow("10.0.0.3") {
		t.Fatal("Allow: request after a Release was rejected")
	}
}

// TestCancelRateLimiter_ZeroMaxPerIPMeansNoPerIPLimit is the round-3
// regression test for the finding that per-source-IP limiting is
// meaningless in this deployment's default topology (every connection
// passes through APISIX and shares its pod IP unless PROXY protocol is
// enabled and trusted) — MaxPerIP <= 0 must disable the per-IP check
// entirely, leaving only the topology-independent global in-flight cap.
func TestCancelRateLimiter_ZeroMaxPerIPMeansNoPerIPLimit(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(time.Minute, 0, 0))

	// The same "IP" (as it would be for every client behind one L4 proxy)
	// must be allowed far past what a real per-IP cap would ever permit.
	for i := 0; i < 20; i++ {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("Allow: request %d rejected with MaxPerIP disabled — a shared source IP must not be throttled", i)
		}
	}
}

func TestCancelRateLimiter_ZeroMaxPerIPStillEnforcesInFlightCap(t *testing.T) {
	l := NewCancelRateLimiter(fixedCancelLimits(time.Minute, 0, 1))

	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: 1st in-flight slot rejected")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("Allow: 2nd concurrent request from the same IP accepted past the global in-flight cap")
	}
}

// TestCancelRateLimiter_LimitsReadPerCall proves the limiter is
// hot-reloadable, like every other limit in design §8.1 — it must read
// current limits on each call rather than capturing them once at
// construction.
func TestCancelRateLimiter_LimitsReadPerCall(t *testing.T) {
	current := CancelLimits{Window: time.Minute, MaxPerIP: 1, MaxInFlight: 0}
	l := NewCancelRateLimiter(func() CancelLimits { return current })

	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: first request rejected")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("Allow: second request within the old limit of 1 was accepted")
	}

	current.MaxPerIP = 10 // simulate a ConfigMap change taking effect
	if !l.Allow("10.0.0.1") {
		t.Fatal("Allow: request after the limit was raised was still rejected — limiter did not re-read current limits")
	}
}

// TestCancelRateLimiter_SweepBoundsMemory is the round-3 regression test
// for the finding that `seen` grew without bound: an IP that appears once
// and never returns must eventually be evicted, bounding memory to
// distinct source IPs active within the last window rather than every IP
// ever seen.
func TestCancelRateLimiter_SweepBoundsMemory(t *testing.T) {
	const window = 20 * time.Millisecond
	l := NewCancelRateLimiter(fixedCancelLimits(window, 5, 0))

	for i := 0; i < 500; i++ {
		l.Allow(randomIP(i))
		l.Release()
	}
	if got := l.seenCount(); got == 0 {
		t.Fatal("seenCount() = 0 immediately after 500 distinct IPs; expected entries before the sweep window elapses")
	}

	time.Sleep(3 * window)
	l.Allow("10.0.0.1") // triggers the amortized sweep
	l.Release()

	if got := l.seenCount(); got > 5 {
		t.Errorf("seenCount() = %d after the sweep window elapsed for 500 stale IPs, want it bounded to only recently-active entries", got)
	}
}

func randomIP(i int) string {
	return fmt.Sprintf("203.0.%d.%d", i/250, i%250)
}
