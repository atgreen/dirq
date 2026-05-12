//go:build !windows

package main

import (
	"log/slog"

	"github.com/atgreen/dirq/internal/agent"
)

// runService is a no-op on non-Windows platforms.
// Returns nil to signal the caller to run in foreground mode.
func runService(_ *slog.Logger, _ agent.Config) error {
	return nil
}
