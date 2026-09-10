package server

import (
	"net"
	"testing"
	"time"
)

// TestListener_TrackAndWaitGroupRace is the round-3 regression test for the
// intermittent `wg.Add(1)`/`wg.Wait()` data race: Serve published a
// connection into l.conns (via track()) and only afterward called
// l.wg.Add(1), outside any lock — so a Shutdown racing in that gap could
// call wg.Wait() while the counter was still zero, violating
// sync.WaitGroup's "a positive-delta Add starting from zero must
// happen-before the corresponding Wait" contract. That shows up under
// -race as a data race on the WaitGroup's internal state, and (worse, if
// unlucky) as Shutdown returning via its "everyone already finished" path
// while a connection is still being handled.
//
// The window is only a couple of instructions wide, so a single iteration
// essentially never hits it — this needs both real iteration count and
// `-race`. Run as `go test -race -count=N ./internal/server/ -run
// TestListener_TrackAndWaitGroupRace` with N in the hundreds for a stable
// repro on the unfixed code; the loop below already iterates internally so
// a single invocation has a realistic chance too.
func TestListener_TrackAndWaitGroupRace(t *testing.T) {
	const iterations = 500

	for i := 0; i < iterations; i++ {
		ln := newLoopbackListener(t)
		addr := ln.Addr().String()
		l := NewListener(ln, longLivedHandler())

		serveErr := make(chan error, 1)
		go func() { serveErr <- l.Serve() }()

		conn, err := net.Dial("tcp", addr)
		if err != nil {
			// The listener wasn't up yet for this iteration; not what
			// we're testing here.
			continue
		}

		// No synchronization between the dial completing and Shutdown
		// starting: Serve may still be between track() and wg.Add() when
		// Shutdown calls wg.Wait() — exactly the gap under test.
		l.Shutdown(200 * time.Millisecond)
		conn.Close()

		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Serve did not return after Shutdown", i)
		}
	}
}
