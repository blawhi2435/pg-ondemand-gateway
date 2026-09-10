// Package sysutil implements the startup RLIMIT_NOFILE bump (design §9.3).
// k8s pod specs have no field for nofile; the effective value comes from
// the container runtime's default and varies across clusters (observed:
// OrbStack soft=20480/hard=1048576; some environments default soft to
// 1024). pg-proxy raises its own soft limit rather than depending on
// cluster-specific configuration nobody remembers to set.
package sysutil

import (
	"fmt"
	"log"
	"syscall"
)

// NofileResult records the RLIMIT_NOFILE soft limit before and after the
// startup adjustment, for logging and the pgproxy_nofile_limit metric.
type NofileResult struct {
	Before uint64
	After  uint64
}

// RaiseNofileLimit reads the current RLIMIT_NOFILE, raises the soft limit
// to the hard limit if it isn't already there, and logs the outcome via
// logger. The symptom of hitting this limit unraised is "accept: too many
// open files" while every other health signal still looks fine — logging
// the before/after values here is what makes that diagnosable later.
func RaiseNofileLimit(logger *log.Logger) (NofileResult, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return NofileResult{}, fmt.Errorf("sysutil: getrlimit(RLIMIT_NOFILE): %w", err)
	}
	before := uint64(lim.Cur)

	if lim.Cur < lim.Max {
		lim.Cur = lim.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			return NofileResult{}, fmt.Errorf("sysutil: setrlimit(RLIMIT_NOFILE) to %d: %w", lim.Max, err)
		}
	}

	result := NofileResult{Before: before, After: uint64(lim.Cur)}
	logger.Printf("sysutil: nofile soft limit %d -> %d (hard limit %d)", result.Before, result.After, lim.Max)
	return result, nil
}
