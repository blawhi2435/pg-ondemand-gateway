package server

import (
	"errors"
	"syscall"
	"testing"
)

// TestIsTemporaryAcceptError_KnownTransientErrnos is the round-3 regression
// test for task 30.6: net.Error.Temporary() is deprecated (it's a poor,
// ever-shrinking proxy for "worth retrying" — see https://go.dev/issue/45729)
// so the Accept retry decision must name the specific transient conditions
// it actually means: the process briefly exhausting its file-descriptor
// budget under a connection-count spike, or the kernel dropping an
// already-queued connection before Accept could hand it over.
func TestIsTemporaryAcceptError_KnownTransientErrnos(t *testing.T) {
	transient := []error{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED}
	for _, err := range transient {
		if !isTemporaryAcceptError(err) {
			t.Errorf("isTemporaryAcceptError(%v) = false, want true", err)
		}
	}
}

func TestIsTemporaryAcceptError_PermanentErrorsAreNotRetried(t *testing.T) {
	permanent := []error{errors.New("simulated permanent accept failure"), syscall.EINVAL}
	for _, err := range permanent {
		if isTemporaryAcceptError(err) {
			t.Errorf("isTemporaryAcceptError(%v) = true, want false", err)
		}
	}
}
