package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := server.Config{
		GRPCAddr: envOr("DIRQ_GRPC_ADDR", ":50051"),
		HTTPAddr: envOr("DIRQ_HTTP_ADDR", ":8080"),
		DBURL:    envOr("DIRQ_DB_URL", "postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable"),
		PodID:    envOr("DIRQ_POD_ID", mustHostname()),
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

	// Start server.
	srv := server.New(cfg, database, log)
	if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
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
