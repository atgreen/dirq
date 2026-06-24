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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

	jitterSecs, _ := strconv.Atoi(config.EnvOr("DIRQ_REGISTRATION_JITTER_SECONDS", fileCfg, "registration_jitter_seconds", "0"))
	if jitterSecs < 0 {
		jitterSecs = 0
	}

	// Agent-side policy (optional). Fail-closed defaults to true once a policy
	// file is configured, so a misconfigured production agent denies rather
	// than silently allows.
	policyFile := config.EnvOr("DIRQ_POLICY_FILE", fileCfg, "policy_file", "")
	failClosedDefault := "false"
	if policyFile != "" {
		failClosedDefault = "true"
	}
	policyFailClosed := config.EnvOr("DIRQ_POLICY_FAIL_CLOSED", fileCfg, "policy_fail_closed", failClosedDefault) == "true"
	policyQuery := config.EnvOr("DIRQ_POLICY_QUERY", fileCfg, "policy_query", "")

	baseCfg := agent.Config{
		ServerAddr:         config.EnvOr("DIRQ_SERVER", fileCfg, "server", "localhost:50051"),
		ListenAddr:         config.EnvOr("DIRQ_LISTEN", fileCfg, "listen", ":50052"),
		Tags:               tags,
		Version:            version,
		ExecEnabled:        execEnabled,
		RegistrationSecret: config.EnvOr("DIRQ_REGISTRATION_SECRET", fileCfg, "registration_secret", ""),
		FileCfg:            fileCfg,
		Hostname:           config.EnvOr("DIRQ_HOSTNAME", fileCfg, "hostname", ""),
		RegistrationJitter: time.Duration(jitterSecs) * time.Second,
		PolicyFile:         policyFile,
		PolicyFailClosed:   policyFailClosed,
		PolicyQuery:        policyQuery,
	}

	// Emulation mode: spawn N virtual-host agents in this process.  Each one
	// presents itself to the server as an independent host (own agent_id,
	// session token, mTLS cert, listen port).  Useful for testing at fleet
	// scale without provisioning a host per agent.
	vhCount := parseVirtualHosts(log, fileCfg)
	if vhCount > 1 {
		runVirtualHosts(log, baseCfg, fileCfg, vhCount)
		return
	}

	log.Info("DirQ agent starting",
		"server", baseCfg.ServerAddr,
		"listen", baseCfg.ListenAddr,
		"version", baseCfg.Version,
		"exec_enabled", baseCfg.ExecEnabled,
	)

	// On Windows, try to run as a service first.
	// Returns nil if we should run in foreground instead.
	if err := runService(log, baseCfg); err != nil {
		log.Error("service error", "error", err)
		os.Exit(1)
	}

	// Foreground mode (Linux always, Windows when not run by SCM).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ag := agent.New(baseCfg, log)
	if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("agent error", "error", err)
		os.Exit(1)
	}
}

// parseVirtualHosts reads DIRQ_VIRTUAL_HOSTS, falling back to the config file.
// Returns 1 (single-tenant) if unset or invalid.
func parseVirtualHosts(log *slog.Logger, fileCfg *config.File) int {
	v := config.EnvOr("DIRQ_VIRTUAL_HOSTS", fileCfg, "virtual_hosts", "1")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Error("invalid DIRQ_VIRTUAL_HOSTS, defaulting to 1", "value", v)
		return 1
	}
	return n
}

// runVirtualHosts launches N agents in this process, each with its own
// synthesized hostname, listen port, and mTLS cert directory.
func runVirtualHosts(log *slog.Logger, base agent.Config, fileCfg *config.File, n int) {
	prefix := strings.TrimRight(config.EnvOr("DIRQ_HOSTNAME_PREFIX", fileCfg, "hostname_prefix", ""), "-")
	if prefix == "" {
		log.Error("DIRQ_VIRTUAL_HOSTS>1 requires DIRQ_HOSTNAME_PREFIX")
		os.Exit(1)
	}
	if err := validateHostnamePrefix(prefix); err != nil {
		log.Error("invalid DIRQ_HOSTNAME_PREFIX", "prefix", prefix, "error", err)
		os.Exit(1)
	}

	host, basePort, err := splitListenAddr(base.ListenAddr)
	if err != nil {
		log.Error("invalid DIRQ_LISTEN for multi-VH mode (need host:port)", "addr", base.ListenAddr, "error", err)
		os.Exit(1)
	}

	width := 5
	if d := digitCount(n - 1); d > width {
		width = d
	}

	// Default to N/4 seconds of jitter (capped at 60s) for emulation runs so
	// 1000 VHs don't all hit Register in the same millisecond.  The user can
	// override (including back to 0) via DIRQ_REGISTRATION_JITTER_SECONDS.
	if base.RegistrationJitter == 0 {
		defaultJitter := time.Duration(n/4) * time.Second
		if defaultJitter > 60*time.Second {
			defaultJitter = 60 * time.Second
		}
		if defaultJitter < 5*time.Second {
			defaultJitter = 5 * time.Second
		}
		base.RegistrationJitter = defaultJitter
	}

	log.Info("DirQ agent starting (multi-VH emulation)",
		"virtual_hosts", n,
		"server", base.ServerAddr,
		"listen_base", base.ListenAddr,
		"hostname_prefix", prefix,
		"jitter_max", base.RegistrationJitter,
		"version", base.Version,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		cfg := base // shallow copy is fine; we don't mutate the tag map
		cfg.Hostname = fmt.Sprintf("%s-%0*d", prefix, width, i)
		cfg.InstanceName = cfg.Hostname
		cfg.ListenAddr = net.JoinHostPort(host, strconv.Itoa(basePort+i))

		instLog := log.With("vh", cfg.Hostname)
		wg.Add(1)
		go func(c agent.Config, l *slog.Logger) {
			defer wg.Done()
			ag := agent.New(c, l)
			if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
				l.Error("agent error", "error", err)
			}
		}(cfg, instLog)
	}
	wg.Wait()
}

// validateHostnamePrefix rejects prefixes that would be unsafe as either an
// advertised hostname or as an on-disk path component (InstanceName is used
// as a directory name in mtlsCertDir).  Allowed: alphanumerics, dot, hyphen,
// and underscore — the conservative intersection of DNS labels and POSIX
// path-safe characters.
func validateHostnamePrefix(p string) error {
	if p == "" {
		return fmt.Errorf("empty")
	}
	if len(p) > 50 {
		return fmt.Errorf("too long (max 50 chars)")
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid character %q (allowed: a-z A-Z 0-9 - _ .)", r)
		}
	}
	return nil
}

// splitListenAddr parses "host:port" into its components, treating ":port"
// as listening on all interfaces.
func splitListenAddr(addr string) (string, int, error) {
	h, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port %q: %w", portStr, err)
	}
	return h, port, nil
}

func digitCount(n int) int {
	if n <= 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
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
