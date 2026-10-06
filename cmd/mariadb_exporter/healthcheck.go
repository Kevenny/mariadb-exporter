package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"
)

// healthcheck queries a running exporter's /health and fails unless it
// answers 200. Credentials embedded in the URL become basic auth; the
// http.Client strips them from error messages.
func healthcheck(url string, insecure bool, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: insecure, //nolint:gosec // opt-in via --insecure-skip-verify
			},
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", resp.StatusCode)
	}
	return nil
}
