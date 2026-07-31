// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/atgreen/dirq/internal/config"
	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/db/postgres"
	"github.com/atgreen/dirq/internal/db/sqlite"
	"github.com/atgreen/dirq/internal/server"
	"github.com/atgreen/dirq/internal/tlsutil"
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
		DBURL:              config.EnvOr("DIRQ_DB_URL", fileCfg, "db_url", "sqlite:///var/lib/dirq/dirq.db"),
		PodID:              config.EnvOr("DIRQ_POD_ID", fileCfg, "pod_id", mustHostname()),
		MaxZoneLeaders:     cfgInt("DIRQ_MAX_ZONE_LEADERS", fileCfg, "max_zone_leaders", 0),
		MaxChildrenPerNode: cfgInt("DIRQ_MAX_CHILDREN", fileCfg, "max_children", 0),
		AuthDisabled:       config.EnvOr("DIRQ_AUTH_DISABLED", fileCfg, "auth_disabled", "false") == "true",
		RequireAAPBinding:  config.EnvOr("DIRQ_REQUIRE_AAP_BINDING", fileCfg, "require_aap_binding", "false") == "true",
		RegistrationSecret: config.EnvOr("DIRQ_REGISTRATION_SECRET", fileCfg, "registration_secret", ""),
		LeaderElection:     config.EnvOr("DIRQ_LEADER_ELECTION", fileCfg, "leader_election", "false") == "true",
		FactFlushInterval:  cfgDur("DIRQ_FACT_FLUSH_INTERVAL", fileCfg, "fact_flush_interval", 0),
		FactFlushSize:      cfgInt("DIRQ_FACT_FLUSH_SIZE", fileCfg, "fact_flush_size", 0),
		FactStageCap:       cfgInt("DIRQ_FACT_STAGE_CAP", fileCfg, "fact_stage_cap", 0),
		// Reboot-aware placement. Fallback -1 means "unset — keep the built-in
		// default"; an explicit 0 is honored (e.g. DIRQ_FLAP_THRESHOLD=0 turns
		// reliability-aware placement off).
		FlapWindow:            cfgDur("DIRQ_FLAP_WINDOW", fileCfg, "flap_window", -1),
		FlapThreshold:         cfgFloat("DIRQ_FLAP_THRESHOLD", fileCfg, "flap_threshold", -1),
		ProbationChildCap:     cfgInt("DIRQ_PROBATION_CHILD_CAP", fileCfg, "probation_child_cap", -1),
		FailureDomainPrefixV4: cfgInt("DIRQ_FAILURE_DOMAIN_PREFIX_V4", fileCfg, "failure_domain_prefix_v4", -1),
		FailureDomainPrefixV6: cfgInt("DIRQ_FAILURE_DOMAIN_PREFIX_V6", fileCfg, "failure_domain_prefix_v6", -1),
		DomainFlapMinNodes:    cfgInt("DIRQ_DOMAIN_FLAP_MIN_NODES", fileCfg, "domain_flap_min_nodes", -1),
		FileCfg:               fileCfg,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Determine backend from URL and connect with retry.
	var database db.DB
	isPostgres := strings.HasPrefix(cfg.DBURL, "postgres://") || strings.HasPrefix(cfg.DBURL, "postgresql://")

	if isPostgres {
		log.Info("using PostgreSQL backend", "url", cfg.DBURL)
	} else {
		log.Info("using SQLite backend", "url", cfg.DBURL)
		// Ensure the directory for the SQLite file exists.
		dsn := strings.TrimPrefix(cfg.DBURL, "sqlite://")
		dir := filepath.Dir(dsn)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Error("failed to create database directory", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	for attempt := 1; ; attempt++ {
		var err error
		if isPostgres {
			database, err = postgres.New(ctx, cfg.DBURL)
		} else {
			database, err = sqlite.New(ctx, cfg.DBURL)
		}
		if err == nil {
			break
		}
		if attempt >= 30 {
			log.Error("failed to connect to database after retries", "error", err)
			os.Exit(1)
		}
		log.Warn("database not ready, retrying...", "attempt", attempt, "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			os.Exit(1)
		}
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
			plaintext, err := database.CreateToken(ctx, "bootstrap", "admin", nil)
			if err != nil {
				log.Error("failed to create bootstrap token", "error", err)
			} else {
				// Write token to a file with restricted permissions instead
				// of logging it, to prevent credential leakage through logs.
				tokenFile := filepath.Join(config.DataDir(), "bootstrap-token")
				if writeErr := os.WriteFile(tokenFile, []byte(plaintext+"\n"), 0600); writeErr != nil {
					log.Error("failed to write bootstrap token file", "error", writeErr)
					// Fall back to logging if file write fails.
					log.Info("bootstrap token: " + plaintext)
				} else {
					log.Info("NO API TOKENS FOUND — bootstrap token created",
						"file", tokenFile)
					log.Info("Read with: cat " + tokenFile)
				}
			}
		}
	} else {
		log.Warn("API authentication disabled (DIRQ_AUTH_DISABLED=true) — NOT RECOMMENDED for production")
	}

	// Write client.conf with server URL and bootstrap token.
	writeClientConfig(cfg, log)

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

// cfgDur parses a duration from env, then config file, then fallback.
func cfgDur(env string, fileCfg *config.File, fileKey string, fallback time.Duration) time.Duration {
	s := config.EnvOr(env, fileCfg, fileKey, "")
	if s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return fallback
}

// cfgFloat parses a float from env, then config file, then fallback.
func cfgFloat(env string, fileCfg *config.File, fileKey string, fallback float64) float64 {
	s := config.EnvOr(env, fileCfg, fileKey, "")
	if s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return fallback
}

func writeClientConfig(cfg server.Config, log *slog.Logger) {
	outPath := filepath.Join(config.DataDir(), "client.conf")

	hostname, _ := os.Hostname()

	// Determine the REST API URL.
	scheme := "https"
	tlsCfg := tlsutil.ConfigFromEnv(cfg.FileCfg)
	if tlsCfg.Disabled {
		scheme = "http"
	}
	// Extract just the port from HTTPAddr (which may be "0.0.0.0:8080" or ":8080").
	httpPort := cfg.HTTPAddr
	if _, port, err := net.SplitHostPort(cfg.HTTPAddr); err == nil {
		httpPort = ":" + port
	}
	serverURL := fmt.Sprintf("%s://%s%s", scheme, hostname, httpPort)

	lines := []string{
		"# DirQ Client Configuration (generated by dirq-server)",
		"# Copy to /etc/dirq/client.conf or ~/.config/dirq/client.conf",
		"",
		"server_url: " + serverURL,
	}

	if tlsCfg.Enabled() && !tlsCfg.HasUserCerts() {
		// Self-signed certs — client needs tls_insecure.
		lines = append(lines, "tls_insecure: true")
	}

	// Include the bootstrap token if it exists.
	tokenFile := filepath.Join(config.DataDir(), "bootstrap-token")
	if tokenData, err := os.ReadFile(tokenFile); err == nil {
		token := strings.TrimSpace(string(tokenData))
		if token != "" {
			lines = append(lines, "token: "+token)
		}
	}

	lines = append(lines, "")
	if err := os.WriteFile(outPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		log.Warn("failed to write client config", "path", outPath, "error", err)
		return
	}
	log.Info("client config written — copy to /etc/dirq/client.conf or ~/.config/dirq/client.conf", "path", outPath)
}

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
