package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && (u != "prom" || p != "s3cret") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	require.NoError(t, healthcheck(srv.URL+"/health", false, time.Second))

	status = http.StatusServiceUnavailable
	require.ErrorContains(t, healthcheck(srv.URL+"/health", false, time.Second), "HTTP 503")

	status = http.StatusOK
	withAuth := strings.Replace(srv.URL, "http://", "http://prom:s3cret@", 1)
	require.NoError(t, healthcheck(withAuth+"/health", false, time.Second), "URL credentials become basic auth")

	wrongAuth := strings.Replace(srv.URL, "http://", "http://prom:wrong@", 1)
	require.ErrorContains(t, healthcheck(wrongAuth+"/health", false, time.Second), "HTTP 401")
}

func TestHealthcheckTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	require.Error(t, healthcheck(srv.URL, false, time.Second), "self-signed certificate is rejected by default")
	require.NoError(t, healthcheck(srv.URL, true, time.Second))
}

// A connection failure must not echo the password embedded in the URL.
func TestHealthcheckErrorDoesNotLeakPassword(t *testing.T) {
	err := healthcheck("http://prom:Sup3rS3cret@127.0.0.1:1/health", false, time.Second)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "Sup3rS3cret")
}
