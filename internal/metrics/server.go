package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTP timeouts for the metrics server. This port can be reachable from
// anywhere on the pod network (if metricsListen binds 0.0.0.0), so a
// connection that opens and never finishes sending headers must not tie up
// a goroutine indefinitely (gosec G112 / Slowloris).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
)

// NewServer builds the HTTP server for metricsListen: /metrics, /healthz,
// /readyz. Design §9.4b requires this on a port independent of :5432 —
// that port speaks PostgreSQL wire protocol, not HTTP — and forbids a TCP
// probe against :5432 itself, so every probe here is a plain HTTP handler.
func NewServer(addr string, m *Metrics, prober *Prober) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", prober.Healthz)
	mux.HandleFunc("/readyz", prober.Readyz)

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}
}
