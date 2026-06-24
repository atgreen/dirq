// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package policy implements optional Open Policy Agent (OPA) / Rego
// enforcement for dirq-agent. It is a last-mile, agent-side policy layer:
// even when a request is validly authorized and routed by the DirQ server,
// the local host can still refuse to perform the action.
//
// The engine is defense in depth. It does not replace server-side API
// authorization, token scopes, message signing, or mTLS. When no policy file
// is configured the engine is a no-op that allows every operation, preserving
// the legacy exec_enabled behavior exactly.
package policy

import (
	"context"
	"time"
)

// DefaultQuery is the Rego decision query evaluated for the allow result.
const DefaultQuery = "data.dirq.agent.allow"

// DefaultEvalTimeout bounds a single policy evaluation. Every local side
// effect blocks on evaluation, so a runaway policy must not hang the agent;
// exceeding this is treated as an evaluation error (fail-open or fail-closed
// per configuration).
const DefaultEvalTimeout = 5 * time.Second

// Decision is the result of evaluating a policy for one operation. It is
// always safe to act on directly: fail-open / fail-closed handling has
// already been applied by the engine.
type Decision struct {
	// Allow reports whether the operation may proceed.
	Allow bool
	// Reason is an optional human-readable explanation, populated on denial.
	// It may be empty even when denied; callers should fall back to a generic
	// "policy denied" message.
	Reason string
}

// Engine evaluates policy for agent operations.
type Engine interface {
	// Eval evaluates the policy for the given input and returns a Decision.
	// The Decision is always actionable. A non-nil error is returned for
	// logging and observability only — it does not need to be acted on
	// because the Decision already reflects the configured failure mode.
	Eval(ctx context.Context, input Input) (Decision, error)

	// Enabled reports whether a policy is actually configured. When false the
	// engine is a no-op and callers may skip building Input entirely.
	Enabled() bool
}

// Config configures engine construction.
type Config struct {
	// File is the path to a local Rego policy file. Empty means no policy
	// (a no-op engine that allows everything).
	File string
	// Query is the Rego decision query. Empty defaults to DefaultQuery.
	Query string
	// FailClosed denies when policy load or evaluation fails. When false,
	// such failures allow the operation (discovery / fail-open mode).
	FailClosed bool
	// EvalTimeout bounds a single evaluation. Zero defaults to DefaultEvalTimeout.
	EvalTimeout time.Duration
}

// New constructs a policy engine from cfg. When cfg.File is empty it returns a
// no-op engine and a nil error. Otherwise it loads and compiles the policy at
// construction time so syntax errors surface before the agent advertises
// itself as ready for exec; a compile failure is returned as an error.
func New(ctx context.Context, cfg Config) (Engine, error) {
	if cfg.File == "" {
		return Nop(), nil
	}
	return newRegoEngine(ctx, cfg)
}

// Nop returns a no-op engine that allows every operation. Used when no policy
// is configured, and as the fail-open fallback when a policy fails to load.
func Nop() Engine { return nopEngine{} }

type nopEngine struct{}

func (nopEngine) Eval(context.Context, Input) (Decision, error) {
	return Decision{Allow: true}, nil
}

func (nopEngine) Enabled() bool { return false }
