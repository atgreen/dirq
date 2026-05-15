// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"net/http"
	"sync"
	"time"
)

// rateLimiter implements a per-key token bucket rate limiter.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   int     // max tokens
}

type bucket struct {
	tokens   float64
	lastTime time.Time
}

func newRateLimiter(ratePerSecond float64, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSecond,
		burst:   burst,
	}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(rl.burst), lastTime: now}
		rl.buckets[key] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastTime = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimitMiddleware wraps a handler with per-token rate limiting.
// Falls back to remote IP if auth is disabled.
func (s *Server) rateLimitMiddleware(rl *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Use the API token as the rate limit key (per-client limiting).
		// Fall back to remote IP if no token (auth disabled).
		key := r.Header.Get("Authorization")
		if key == "" {
			key = r.RemoteAddr
		}
		if !rl.allow(key) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
