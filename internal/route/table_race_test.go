package route

import (
	"fmt"
	"sync"
	"testing"
)

// TestTable_ConcurrentRebuildAndLookup exercises Rebuild and Lookup from many
// goroutines at once; run with -race to catch a concurrent map read/write.
func TestTable_ConcurrentRebuildAndLookup(t *testing.T) {
	tbl := NewTable()
	rec := &fakeEventRecorder{}

	const goroutines = 50
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				tbl.Rebuild([]PoolerInfo{
					{
						Namespace: "pgproxy-e2e",
						Name:      fmt.Sprintf("tenant%d-pooler", i),
						Hostname:  fmt.Sprintf("tenant%d.db.test", i),
						Cluster:   fmt.Sprintf("tenant%d", i),
					},
				}, rec)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				tbl.Lookup(fmt.Sprintf("tenant%d.db.test", i))
			}
		}()
	}
	wg.Wait()
}
