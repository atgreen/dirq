// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ─────────────────────────────────────────────────────────
// HTTP client helpers
// ─────────────────────────────────────────────────────────

// httpClient returns a shared HTTP client, creating it once on first use.
// When --tls-insecure is set, the client skips certificate verification
// but still reuses connections (unlike the old code which created a new
// client + transport on every request).
var _httpClient *http.Client

// tlsRoots holds the CA pool built from --tls-ca, when one was given.
var tlsRoots *x509.CertPool

// loadTLSRoots reads the --tls-ca file into a certificate pool. An
// unreadable or unparseable CA is a hard error: falling back to the system
// trust store would silently do something other than what was asked, and
// the failure would look like a server problem later.
func loadTLSRoots(insecureAsked bool) error {
	if tlsCA == "" {
		return nil
	}
	pem, err := os.ReadFile(tlsCA)
	if err != nil {
		return fmt.Errorf("read --tls-ca %s: %w", tlsCA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("--tls-ca %s contains no PEM certificates", tlsCA)
	}
	tlsRoots = pool

	if tlsInsecure {
		// Both were set. Verifying against a named CA is what the more
		// specific option asked for, so honour it — and say so only when
		// insecure was asked for deliberately rather than inherited from
		// a config file, so the warning stays worth reading.
		if insecureAsked {
			fmt.Fprintln(os.Stderr, "dirq: --tls-ca given, so --tls-insecure is ignored and the server certificate will be verified")
		}
		tlsInsecure = false
	}
	return nil
}

func httpClient() *http.Client {
	if _httpClient == nil {
		switch {
		case tlsRoots != nil:
			_httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{RootCAs: tlsRoots, MinVersion: tls.VersionTLS12},
				},
			}
		case tlsInsecure:
			_httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		default:
			_httpClient = http.DefaultClient
		}
	}
	return _httpClient
}

// apiStreamRequest returns the raw HTTP response for streaming (caller must close Body).
func apiStreamRequest(method, path string, body io.Reader) (*http.Response, error) {
	url := strings.TrimRight(serverURL, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func apiRequest(method, path string, body io.Reader) ([]byte, error) {
	resp, err := apiStreamRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}
