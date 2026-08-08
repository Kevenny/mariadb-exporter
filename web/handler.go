// Package web contém os handlers HTTP do exporter: /metrics, /health e a página
// de índice.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// healthTimeout limita o ping usado pelo endpoint /health.
const healthTimeout = 5 * time.Second

// Pinger é implementado pelo exporter e usado pelo /health para checar a conexão.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Options configura o mux HTTP.
type Options struct {
	TelemetryPath string
	MaxRequests   int
	Registry      *prometheus.Registry
	Pinger        Pinger
	Version       string
	Logger        log.Logger
}

// NewHandler monta o mux com /metrics, /health e a página de índice.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()

	telemetryPath := opts.TelemetryPath
	if telemetryPath == "" {
		telemetryPath = "/metrics"
	}

	metricsHandler := promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{
		ErrorLog:            promLogger{opts.Logger},
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: opts.MaxRequests,
	})
	mux.Handle(telemetryPath, metricsHandler)

	mux.HandleFunc("/health", healthHandler(opts.Pinger, opts.Logger))
	mux.HandleFunc("/", indexHandler(telemetryPath, opts.Version))

	return mux
}

// healthHandler responde 200 quando o banco está acessível e 503 caso contrário
// (seção 2.3).
func healthHandler(pinger Pinger, logger log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if pinger == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":  "error",
				"message": "exporter não inicializado",
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		if err := pinger.Ping(ctx); err != nil {
			// O detalhe do erro fica só no log: /health não tem autenticação, e
			// a mensagem do driver pode conter o DSN inteiro (com senha) ou
			// revelar endereços e portas da rede interna.
			_ = level.Warn(logger).Log("msg", "health check falhou", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":  "error",
				"message": "banco de dados inacessível; consulte os logs do exporter",
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="pt-br">
<head><meta charset="utf-8"><title>MariaDB Exporter</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem auto;max-width:44rem;line-height:1.5;color:#1b1b1b}
a{color:#0b6bcb}code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}
</style></head>
<body>
<h1>MariaDB Exporter</h1>
<p>Versão <code>{{.Version}}</code></p>
<ul>
  <li><a href="{{.TelemetryPath}}">{{.TelemetryPath}}</a> — métricas Prometheus</li>
  <li><a href="/health">/health</a> — status da conexão com o banco</li>
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

// promLogger adapta o log.Logger do go-kit à interface esperada por promhttp.
type promLogger struct {
	logger log.Logger
}

func (l promLogger) Println(v ...interface{}) {
	if l.logger == nil {
		return
	}
	_ = level.Error(l.logger).Log("msg", fmt.Sprint(v...))
}
