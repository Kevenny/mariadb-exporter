// Package web contains the exporter's HTTP handlers: /metrics, /health, and
// the index page.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// healthTimeout limits the ping used by the /health endpoint.
const healthTimeout = 5 * time.Second

// Pinger is implemented by the exporter and used by /health to check the connection.
type Pinger interface {
	Ping(ctx context.Context) error
}

// scrapeTimeoutOffset is subtracted from Prometheus's scrape timeout so the
// exporter answers with partial data before Prometheus gives up on it.
const scrapeTimeoutOffset = 250 * time.Millisecond

// Options configures the HTTP mux.
type Options struct {
	TelemetryPath string
	MaxRequests   int
	// Registry holds collectors that don't touch the database (build info,
	// Go and process metrics).
	Registry *prometheus.Registry
	// Scrape returns the database collector bound to the request's context.
	Scrape  func(context.Context) prometheus.Collector
	Pinger  Pinger
	Version string
	Logger  log.Logger
}

// NewHandler assembles the mux with /metrics, /health, and the index page.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()

	telemetryPath := opts.TelemetryPath
	if telemetryPath == "" {
		telemetryPath = "/metrics"
	}

	mux.Handle(telemetryPath, limitInFlight(opts.MaxRequests, metricsHandler(opts)))
	mux.HandleFunc("/health", healthHandler(opts.Pinger, opts.Logger))
	mux.HandleFunc("/", indexHandler(telemetryPath, opts.Version))

	return mux
}

// metricsHandler gathers a per-request registry so the database collector
// runs under the request's context: a scrape Prometheus abandoned (client
// disconnect or its scrape timeout) cancels the queries instead of letting
// them hold the connection pool.
func metricsHandler(opts Options) http.Handler {
	handlerOpts := promhttp.HandlerOpts{
		ErrorLog:      promLogger{opts.Logger},
		ErrorHandling: promhttp.ContinueOnError,
	}

	if opts.Scrape == nil {
		return promhttp.HandlerFor(opts.Registry, handlerOpts)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if timeout, ok := scrapeTimeout(r); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		scoped := prometheus.NewRegistry()
		scoped.MustRegister(opts.Scrape(ctx))

		promhttp.HandlerFor(prometheus.Gatherers{opts.Registry, scoped}, handlerOpts).ServeHTTP(w, r)
	})
}

// scrapeTimeout reads the deadline Prometheus announces for the scrape.
func scrapeTimeout(r *http.Request) (time.Duration, bool) {
	v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds")
	if v == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, false
	}
	timeout := time.Duration(seconds*float64(time.Second)) - scrapeTimeoutOffset
	if timeout <= 0 {
		return 0, false
	}
	return timeout, true
}

// limitInFlight rejects scrapes beyond max with 503, so a burst of requests
// can't pile up goroutines waiting for the small connection pool.
func limitInFlight(max int, next http.Handler) http.Handler {
	if max <= 0 {
		return next
	}
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, fmt.Sprintf("too many concurrent scrapes (limit %d)", max), http.StatusServiceUnavailable)
		}
	})
}

// healthHandler responds 200 when the database is reachable and 503
// otherwise (section 2.3).
func healthHandler(pinger Pinger, logger log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if pinger == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":  "error",
				"message": "exporter not initialized",
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		if err := pinger.Ping(ctx); err != nil {
			// The error detail stays only in the log: /health has no
			// authentication, and the driver's message may contain the full
			// DSN (with password) or reveal internal network addresses and
			// ports.
			_ = level.Warn(logger).Log("msg", "health check failed", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":  "error",
				"message": "database unreachable; check the exporter logs",
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>MariaDB Exporter</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem auto;max-width:44rem;line-height:1.5;color:#1b1b1b}
a{color:#0b6bcb}code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}
</style></head>
<body>
<h1>MariaDB Exporter</h1>
<p>Version <code>{{.Version}}</code></p>
<ul>
  <li><a href="{{.TelemetryPath}}">{{.TelemetryPath}}</a> — Prometheus metrics</li>
  <li><a href="/health">/health</a> — database connection status</li>
</ul>
</body>
</html>
`))

func indexHandler(telemetryPath, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, struct {
			TelemetryPath string
			Version       string
		}{telemetryPath, version})
	}
}

// promLogger adapts go-kit's log.Logger to the interface expected by promhttp.
type promLogger struct {
	logger log.Logger
}

func (l promLogger) Println(v ...interface{}) {
	if l.logger == nil {
		return
	}
	_ = level.Error(l.logger).Log("msg", fmt.Sprint(v...))
}
