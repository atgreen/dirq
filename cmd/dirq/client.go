// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
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

func httpClient() *http.Client {
	if _httpClient == nil {
		if tlsInsecure {
			_httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		} else {
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
