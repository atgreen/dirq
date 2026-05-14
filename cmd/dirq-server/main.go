// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/atgreen/dirq/internal/config"
	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Load config file (missing file is fine — returns empty config).
	cfgPath := os.Getenv("DIRQ_CONFIG")
	if cfgPath == "" {
		cfgPath = config.DefaultServerPath()
	}
	fileCfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("failed to load config file", "path", cfgPath, "error", err)
		os.Exit(1)
	}

	cfg := server.Config{
		GRPCAddr:           config.EnvOr("DIRQ_GRPC_ADDR", fileCfg, "grpc_addr", ":50051"),
		HTTPAddr:           config.EnvOr("DIRQ_HTTP_ADDR", fileCfg, "http_addr", ":8080"),
		DBURL:              config.EnvOr("DIRQ_DB_URL", fileCfg, "db_url", "postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable"),
		PodID:              config.EnvOr("DIRQ_POD_ID", fileCfg, "pod_id", mustHostname()),
		MaxZoneLeaders:     cfgInt("DIRQ_MAX_ZONE_LEADERS", fileCfg, "max_zone_leaders", 0),
		MaxChildrenPerNode: cfgInt("DIRQ_MAX_CHILDREN", fileCfg, "max_children", 0),
		AuthDisabled:       config.EnvOr("DIRQ_AUTH_DISABLED", fileCfg, "auth_disabled", "false") == "true",
		FileCfg:            fileCfg,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Connect to database.
	database, err := db.New(ctx, cfg.DBURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// Run migrations.
	if err := database.RunMigrations(ctx); err != nil {
		log.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	log.Info("database migrations complete")

	// Bootstrap: if no API tokens exist and auth is enabled, create one.
	if !cfg.AuthDisabled {
		tokens, _ := database.ListTokens(ctx)
		if len(tokens) == 0 {
			plaintext, err := database.CreateToken(ctx, "bootstrap", "admin")
			if err != nil {
				log.Error("failed to create bootstrap token", "error", err)
			} else {
				log.Info("========================================")
				log.Info("NO API TOKENS FOUND — bootstrap token created")
				log.Info("Save this token — it cannot be retrieved later:")
				log.Info("  " + plaintext)
				log.Info("Use: export DIRQ_TOKEN=" + plaintext)
				log.Info("========================================")
			}
		}
	} else {
		log.Warn("API authentication disabled (DIRQ_AUTH_DISABLED=true) — NOT RECOMMENDED for production")
	}

	// Start server.
	srv := server.New(cfg, database, log)
	if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}

// cfgInt returns env var as int, then config file value, then fallback.
func cfgInt(env string, fileCfg *config.File, fileKey string, fallback int) int {
	s := config.EnvOr(env, fileCfg, fileKey, "")
	if s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return fallback
}

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
