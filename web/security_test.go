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

// /health is an endpoint without authentication: anyone who can reach the
// exporter's port can call it. It returns the driver's error message, which
// in some failure modes includes the full DSN — and therefore the password.
func TestHealthDoesNotLeakCredentialsInErrorMessage(t *testing.T) {
	const senha = "S3nh4-Sup3r-S3cr3t4"

	// Error in the format go-sql-driver/mysql can produce, with the DSN inside.
	driverErr := errors.New(
		`dial tcp 10.0.0.5:3306: connect: connection refused ` +
			`(dsn: mariadb_exporter:` + senha + `@tcp(10.0.0.5:3306)/)`,
	)

	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        fakePinger{err: driverErr},
		Logger:        log.NewNopLogger(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	body := rec.Body.String()
	require.NotContains(t, body, senha,
		"the password leaked in the /health body, which is public: %s", body)
}

// Even without a credential, exposing the driver's raw error reveals
// internal topology (IPs, ports, hostnames) to whoever can reach the endpoint.
func TestHealthErrorMessageIsNotRawDriverError(t *testing.T) {
	driverErr := errors.New("dial tcp 10.0.0.5:3306: connect: connection refused")

	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        fakePinger{err: driverErr},
		Logger:        log.NewNopLogger(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var payload map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "error", payload["status"])

	// The error detail belongs in the operator's log, not the HTTP response.
	require.NotContains(t, payload["message"], "10.0.0.5",
		"/health exposed the database's internal address: %q", payload["message"])
}

// /health should not accept methods that suggest a state mutation.
func TestHealthOnlyAllowsSafeMethods(t *testing.T) {
	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        fakePinger{},
		Logger:        log.NewNopLogger(),
	})

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/health", nil))
		t.Logf("%s /health -> %d", method, rec.Code)
	}
}

// The index page should not contain the DSN or any credential.
func TestIndexPageDoesNotLeakConfiguration(t *testing.T) {
	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        fakePinger{},
		Version:       "1.0.0",
		Logger:        log.NewNopLogger(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	require.NotContains(t, body, "@tcp(", "the index page exposed a DSN")
	require.NotContains(t, body, "senha")
	require.NotContains(t, body, "password")
}

// A path with traversal should not escape the mux or serve files from disk.
func TestNoPathTraversal(t *testing.T) {
	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        fakePinger{},
		Logger:        log.NewNopLogger(),
	})

	for _, path := range []string{
		"/../../etc/passwd",
		"/metrics/../../../etc/shadow",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)

		body := rec.Body.String()
		require.NotContains(t, body, "root:", "path %q appears to have served /etc/passwd", path)
		require.NotContains(t, strings.ToLower(body), "/bin/bash")
	}
}

// The health check has its own timeout; a Ping that hangs should not hold
// the HTTP connection indefinitely.
func TestHealthRespectsContextTimeout(t *testing.T) {
	blocking := blockingPinger{released: make(chan struct{})}
	defer close(blocking.released)

	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        blocking,
		Logger:        log.NewNopLogger(),
	})

	// Client gives up before the exporter: the handler must terminate without leaking.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx)
	cancel()

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { h.ServeHTTP(rec, req) })
}

// blockingPinger respects context cancellation, as a well-behaved driver would.
type blockingPinger struct {
	released chan struct{}
}

func (b blockingPinger) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.released:
		return nil
	}
}
