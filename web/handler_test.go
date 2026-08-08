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

// fakePinger simulates the state of the database connection.
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

// Connected: 200 with {"status":"ok"} (section 2.3).
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

// Disconnected: 503 with status error and a generic message.
//
// The message is deliberately generic: /health has no authentication, and
// the driver's error may contain the DSN (with password) or internal
// network addresses. The detail stays in the exporter's log.
func TestHealthEndpointUnavailable(t *testing.T) {
	h := newTestHandler(t, fakePinger{err: errors.New("connection refused")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "error", body["status"])
	require.NotEmpty(t, body["message"])
	require.NotContains(t, body["message"], "connection refused",
		"the driver's error should not appear in the public response")
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

// Unknown paths should give 404 instead of serving the index page.
func TestUnknownPathReturns404(t *testing.T) {
	h := newTestHandler(t, fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nao-existe", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// The metrics path is configurable.
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
	require.NotPanics(t, func() { l.Println("some error") })
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
		"html/template should escape the version")
}
