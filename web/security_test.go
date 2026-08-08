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

// O /health é um endpoint sem autenticação: qualquer um que alcance a porta do
// exporter pode chamá-lo. Ele devolve a mensagem de erro do driver, que em
// alguns modos de falha inclui o DSN completo — e portanto a senha.
func TestHealthDoesNotLeakCredentialsInErrorMessage(t *testing.T) {
	const senha = "S3nh4-Sup3r-S3cr3t4"

	// Erro no formato que o go-sql-driver/mysql pode produzir, com o DSN dentro.
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
		"a senha vazou no corpo do /health, que é público: %s", body)
}

// Mesmo sem credencial, expor o erro cru do driver revela topologia interna
// (IPs, portas, nomes de host) para quem quer que alcance o endpoint.
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

	// O detalhe do erro pertence ao log do operador, não à resposta HTTP.
	require.NotContains(t, payload["message"], "10.0.0.5",
		"o /health expôs o endereço interno do banco: %q", payload["message"])
}

// O /health não deve aceitar métodos que sugiram mutação de estado.
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

// A página de índice não deve conter o DSN nem qualquer credencial.
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
	require.NotContains(t, body, "@tcp(", "a página de índice expôs um DSN")
	require.NotContains(t, body, "senha")
	require.NotContains(t, body, "password")
}

// Um path com traversal não deve escapar do mux nem servir arquivos do disco.
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
		require.NotContains(t, body, "root:", "path %q parece ter servido /etc/passwd", path)
		require.NotContains(t, strings.ToLower(body), "/bin/bash")
	}
}

// O health check tem um timeout próprio; um Ping que trava não deve segurar a
// conexão HTTP indefinidamente.
func TestHealthRespectsContextTimeout(t *testing.T) {
	blocking := blockingPinger{released: make(chan struct{})}
	defer close(blocking.released)

	h := NewHandler(Options{
		TelemetryPath: "/metrics",
		Registry:      prometheus.NewRegistry(),
		Pinger:        blocking,
		Logger:        log.NewNopLogger(),
	})

	// Cliente desiste antes do exporter: o handler deve terminar sem vazar.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx)
	cancel()

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { h.ServeHTTP(rec, req) })
}

// blockingPinger respeita o cancelamento do contexto, como um driver bem
// comportado faria.
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
