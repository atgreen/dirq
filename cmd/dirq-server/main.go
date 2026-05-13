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

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := server.Config{
		GRPCAddr:           envOr("DIRQ_GRPC_ADDR", ":50051"),
		HTTPAddr:           envOr("DIRQ_HTTP_ADDR", ":8080"),
		DBURL:              envOr("DIRQ_DB_URL", "postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable"),
		PodID:              envOr("DIRQ_POD_ID", mustHostname()),
		MaxZoneLeaders:     envInt("DIRQ_MAX_ZONE_LEADERS", 0),
		MaxChildrenPerNode: envInt("DIRQ_MAX_CHILDREN", 0),
		AuthDisabled:       os.Getenv("DIRQ_AUTH_DISABLED") == "true",
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

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
