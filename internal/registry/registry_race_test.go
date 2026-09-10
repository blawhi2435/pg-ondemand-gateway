package registry

import (
	"fmt"
	"sync"
	"testing"
)

// TestRegistry_ConcurrentAddRemove exercises Add/Remove/Snapshot/Count from
// many goroutines at once; run with -race to catch unsynchronized access.
func TestRegistry_ConcurrentAddRemove(t *testing.T) {
	r := NewInMemoryRegistry(fixedLimits(0, 0))

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("conn-%d", i)
			cluster := fmt.Sprintf("tenant%d", i%3)
			if err := r.Add(testConn(id, cluster)); err != nil {
				t.Errorf("Add: %v", err)
				return
			}
			r.Snapshot()
			r.Count()
			r.Remove(id)
		}()
	}
	wg.Wait()

	total, _ := r.Count()
	if total != 0 {
		t.Fatalf("total after concurrent add/remove = %d, want 0", total)
	}
}
