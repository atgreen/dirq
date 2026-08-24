// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// llmIsAnthropic reports whether the URL points to Anthropic's native API
// rather than an OpenAI-compatible endpoint.
func llmIsAnthropic(apiURL string) bool {
	return strings.Contains(apiURL, "anthropic.com")
}

// llmRequest posts a chat payload to the LLM endpoint, handling the
// dialect differences between Anthropic's native API (/v1/messages,
// x-api-key) and OpenAI-compatible endpoints (/chat/completions, Bearer
// auth), and returns the raw response body. The caller builds the payload
// in the matching dialect.
func llmRequest(ctx context.Context, apiURL, apiKey string, payload map[string]any) ([]byte, error) {
	path := "/chat/completions"
	if llmIsAnthropic(apiURL) {
		path = "/v1/messages"
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(apiURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if llmIsAnthropic(apiURL) {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read LLM response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("LLM API returned HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}
