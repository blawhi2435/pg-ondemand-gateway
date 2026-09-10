// Package tlsutil implements pg-proxy's outward-facing certificate hot
// reload (design §8.2). The mechanism mirrors internal/config: periodic
// reread, SHA-256 compare, atomic.Pointer swap — not fsnotify, for the same
// reason (a k8s Secret mount updates via a symlink swap, not a file write).
package tlsutil

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"
)

// DefaultReloadInterval matches design §8.1's tls.reloadInterval default.
const DefaultReloadInterval = 30 * time.Second

// ErrNoCertificate is returned by GetCertificate if called before any
// certificate has ever loaded successfully — it should be unreachable in
// practice since NewStore requires an initial successful load.
var ErrNoCertificate = errors.New("tlsutil: no certificate loaded")

// Store holds the currently-effective TLS certificate behind an
// atomic.Pointer. Its GetCertificate method is meant to be plugged directly
// into tls.Config.GetCertificate, which is called on every handshake — so
// existing connections (already past their handshake) are unaffected by a
// later reload, while every new connection sees the current certificate.
type Store struct {
	certFile, keyFile string
	interval          time.Duration
	cert              atomic.Pointer[tls.Certificate]
	lastHash          atomic.Pointer[[sha256.Size]byte]
}

// NewStore loads certFile/keyFile once, reloading at DefaultReloadInterval.
// A failure here is fatal — there is no prior good certificate to fall back
// to yet.
func NewStore(certFile, keyFile string) (*Store, error) {
	return NewStoreWithInterval(certFile, keyFile, DefaultReloadInterval)
}

// NewStoreWithInterval is NewStore with an explicit reload cadence — this
// is what makes design §8.1's tls.reloadInterval config key take effect
// rather than being validated and then silently ignored. interval <= 0
// falls back to DefaultReloadInterval.
func NewStoreWithInterval(certFile, keyFile string, interval time.Duration) (*Store, error) {
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	s := &Store{certFile: certFile, keyFile: keyFile, interval: interval}
	if _, err := s.ReloadNow(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetCertificate implements the tls.Config.GetCertificate signature.
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := s.cert.Load()
	if cert == nil {
		return nil, ErrNoCertificate
	}
	return cert, nil
}

// NotAfter returns the current certificate's expiry, for the
// pgproxy_cert_expiry_seconds metric. Zero value if no certificate loaded.
func (s *Store) NotAfter() time.Time {
	cert := s.cert.Load()
	if cert == nil || cert.Leaf == nil {
		return time.Time{}
	}
	return cert.Leaf.NotAfter
}

// ReloadNow re-reads certFile/keyFile once, synchronously. changed reports
// whether their combined content hash differed from the last load. An
// error (read, parse, or key-mismatch failure) leaves the serving
// certificate unchanged — a reload must never replace a working
// certificate with a broken one.
func (s *Store) ReloadNow() (changed bool, err error) {
	certPEM, err := os.ReadFile(s.certFile)
	if err != nil {
		return false, fmt.Errorf("tlsutil: read %s: %w", s.certFile, err)
	}
	keyPEM, err := os.ReadFile(s.keyFile)
	if err != nil {
		return false, fmt.Errorf("tlsutil: read %s: %w", s.keyFile, err)
	}

	hash := sha256.Sum256(append(certPEM, keyPEM...))
	if last := s.lastHash.Load(); last != nil && hash == *last {
		return false, nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("tlsutil: reload rejected, keeping previous certificate: %w", err)
	}
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return false, fmt.Errorf("tlsutil: parse leaf certificate, keeping previous certificate: %w", err)
	}

	s.lastHash.Store(&hash)
	s.cert.Store(&cert)
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
				log.Printf("tlsutil: reload failed: %v", err)
				continue
			}
			if changed {
				log.Printf("tlsutil: certificate reloaded from %s", s.certFile)
			}
		}
	}
}
