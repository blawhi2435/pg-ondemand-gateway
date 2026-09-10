package sysutil

import (
	"bytes"
	"log"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestRaiseNofileLimit_SoftReachesHard(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	result, err := RaiseNofileLimit(logger)
	if err != nil {
		t.Fatalf("RaiseNofileLimit: %v", err)
	}

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	if uint64(lim.Cur) != uint64(lim.Max) {
		t.Errorf("soft limit = %d, hard limit = %d; want soft raised to hard", lim.Cur, lim.Max)
	}
	if result.After != uint64(lim.Max) {
		t.Errorf("result.After = %d, want %d (the hard limit)", result.After, lim.Max)
	}
}

func TestRaiseNofileLimit_ResultIsLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	result, err := RaiseNofileLimit(logger)
	if err != nil {
		t.Fatalf("RaiseNofileLimit: %v", err)
	}

	logged := buf.String()
	for _, want := range []string{"nofile"} {
		if !strings.Contains(strings.ToLower(logged), want) {
			t.Errorf("log output %q missing %q", logged, want)
		}
	}
	if !strings.Contains(logged, strconv.FormatUint(result.Before, 10)) {
		t.Errorf("log output %q missing before value %d", logged, result.Before)
	}
	if !strings.Contains(logged, strconv.FormatUint(result.After, 10)) {
		t.Errorf("log output %q missing after value %d", logged, result.After)
	}
}
