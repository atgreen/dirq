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
	"github.com/atgreen/dirq/internal/config"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Load config file (missing file is fine — returns empty config).
	cfgPath := os.Getenv("DIRQ_CONFIG")
	if cfgPath == "" {
		cfgPath = config.DefaultAgentPath()
	}
	fileCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("failed to load config file", "path", cfgPath, "error", err)
		os.Exit(1)
	}

	// Build agent config: env vars override config file, which overrides defaults.
	tags := mergeTags(fileCfg.GetTags(), os.Getenv("DIRQ_TAGS"))

	execEnabled := config.EnvOr("DIRQ_EXEC_ENABLED", fileCfg, "exec_enabled", "false") == "true"

	cfg := agent.Config{
		ServerAddr:  config.EnvOr("DIRQ_SERVER", fileCfg, "server", "localhost:50051"),
		ListenAddr:  config.EnvOr("DIRQ_LISTEN", fileCfg, "listen", ":50052"),
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

// mergeTags combines config file tags with DIRQ_TAGS env var.
// Env var tags override file tags for the same key.
func mergeTags(fileTags map[string]string, envTagStr string) map[string]string {
	tags := make(map[string]string)
	for k, v := range fileTags {
		tags[k] = v
	}
	// Env var overrides.
	for k, v := range parseTags(envTagStr) {
		tags[k] = v
	}
	return tags
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
