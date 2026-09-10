package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"
)

// DefaultReloadInterval matches the TLS cert reload cadence described in
// design §8.1 — there's no dedicated config-reload key in the schema, so
// both share the same default cadence.
const DefaultReloadInterval = 30 * time.Second

func readFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return raw, nil
}

// Store holds the currently-effective Config behind an atomic.Pointer and
// knows how to reload it from disk.
//
// Reload is periodic-reread-plus-hash-compare, not fsnotify: k8s updates a
// mounted ConfigMap by creating a new "..data" directory and atomically
// swapping a symlink to it, which a watch on the file itself never sees as
// a write event. A silently-stale config is worse than the reload
// interval's added latency, and kubelet's own sync loop already runs on a
// ~60s cadence, so there's nothing to gain from event-driven reload here.
type Store struct {
	path     string
	interval time.Duration
	current  atomic.Pointer[Config]
	lastHash atomic.Pointer[[sha256.Size]byte]
}

// NewStore loads path once (returning an error if that fails, since there's
// no prior good value to fall back to) and returns a Store ready to serve
// Current and be reloaded.
func NewStore(path string) (*Store, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}

	s := &Store{path: path, interval: DefaultReloadInterval}
	s.current.Store(cfg)
	hash := sha256.Sum256(raw)
	s.lastHash.Store(&hash)
	return s, nil
}

// Current returns the currently-effective Config. Safe to call from any
// goroutine; the returned pointer is never mutated in place.
func (s *Store) Current() *Config {
	return s.current.Load()
}

// ReloadNow re-reads the file once, synchronously. changed reports whether
// the content hash differed from the last load. An error (read failure,
// parse failure, or a validation failure) leaves Current() unchanged.
func (s *Store) ReloadNow() (changed bool, err error) {
	raw, err := readFile(s.path)
	if err != nil {
		return false, err
	}

	hash := sha256.Sum256(raw)
	if hash == *s.lastHash.Load() {
		return false, nil
	}

	cfg, err := parse(raw)
	if err != nil {
		return false, fmt.Errorf("config: reload of %s rejected, keeping previous config: %w", s.path, err)
	}

	s.lastHash.Store(&hash)
	s.current.Store(cfg)
	return true, nil
}

// Run periodically calls ReloadNow until ctx is cancelled, logging changes
// and errors. It's the production entry point; tests call ReloadNow directly.
func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := s.ReloadNow()
			if err != nil {
				log.Printf("config: reload failed: %v", err)
				continue
			}
			if changed {
				log.Printf("config: reloaded %s", s.path)
			}
		}
	}
}
