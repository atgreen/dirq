// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/atgreen/dirq/internal/agent"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tags := parseTags(os.Getenv("DIRQ_TAGS"))

	execEnabled := envOr("DIRQ_EXEC_ENABLED", "false") == "true"

	cfg := agent.Config{
		ServerAddr:  envOr("DIRQ_SERVER", "localhost:50051"),
		ListenAddr:  envOr("DIRQ_LISTEN", ":50052"),
		Tags:        tags,
		Version:     version,
		ExecEnabled: execEnabled,
	}

	log.Info("DirQ agent starting",
		"server", cfg.ServerAddr,
		"listen", cfg.ListenAddr,
		"version", cfg.Version,
		"exec_enabled", cfg.ExecEnabled,
	)

	// On Windows, try to run as a service first.
	// Returns nil if we should run in foreground instead.
	if err := runService(log, cfg); err != nil {
		log.Error("service error", "error", err)
		os.Exit(1)
	}

	// Foreground mode (Linux always, Windows when not run by SCM).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ag := agent.New(cfg, log)
	if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("agent error", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseTags parses "key=value,key2=value2" into a map.
func parseTags(s string) map[string]string {
	tags := make(map[string]string)
	if s == "" {
		return tags
	}
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			tags[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return tags
}
