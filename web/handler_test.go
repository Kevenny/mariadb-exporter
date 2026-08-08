package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// fakePinger simula o estado da conexão com o banco.
type fakePinger struct {
	err error
}

func (f fakePinger) Ping(context.Context) error { return f.err }

func newTestHandler(t *testing.T, pinger Pinger) http.Handler {
	t.Helper()

	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "mariadb_up", Help: "teste"})
	g.Set(1)
	reg.MustRegister(g)

	return NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      reg,
		Pinger:        pinger,
		Version:       "1.0.0-teste",
		Logger:        log.NewNopLogger(),
	})
}

// Conectado: 200 com {"status":"ok"} (seção 2.3).
func TestHealthEndpointOK(t *testing.T) {
	h := newTestHandler(t, fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body["status"])
}

// Desconectado: 503 com status error e mensagem.
func TestHealthEndpointUnavailable(t *testing.T) {
	h := newTestHandler(t, fakePinger{err: errors.New("connection refused")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "error", body["status"])
	require.Contains(t, body["message"], "connection refused")
}

func TestHealthEndpointNilPinger(t *testing.T) {
	h := newTestHandler(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestMetricsEndpoint(t *testing.T) {
	h := newTestHandler(t, fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "mariadb_up 1")
}

func TestIndexEndpoint(t *testing.T) {
	h := newTestHandler(t, fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	require.Contains(t, body, "MariaDB Exporter")
	require.Contains(t, body, "/metrics")
	require.Contains(t, body, "/health")
	require.Contains(t, body, "1.0.0-teste")
}

// Paths desconhecidos devem dar 404 em vez de servirem a página de índice.
func TestUnknownPathReturns404(t *testing.T) {
	h := newTestHandler(t, fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nao-existe", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// O path das métricas é configurável.
func TestCustomTelemetryPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHandler(Options{
		TelemetryPath: "/custom-metrics",
		Registry:      reg,
		Pinger:        fakePinger{},
		Logger:        log.NewNopLogger(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/custom-metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Contains(t, rec.Body.String(), "/custom-metrics")
}

func TestPromLoggerDoesNotPanicWithNilLogger(t *testing.T) {
	l := promLogger{}
	require.NotPanics(t, func() { l.Println("erro qualquer") })
}

func TestIndexEscapesVersion(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      reg,
		Pinger:        fakePinger{},
		Version:       `<script>alert(1)</script>`,
		Logger:        log.NewNopLogger(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.False(t, strings.Contains(rec.Body.String(), "<script>alert(1)</script>"),
		"html/template deve escapar a versão")
}
