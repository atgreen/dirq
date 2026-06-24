// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package policy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"
)

// failClosedReason is the denial reason surfaced when a fail-closed engine
// hits a load or evaluation error.
const failClosedReason = "policy evaluation error"

// regoEngine is an Engine backed by a compiled Rego policy.
type regoEngine struct {
	allow       rego.PreparedEvalQuery
	reason      *rego.PreparedEvalQuery // nil if the reason rule isn't queryable
	failClosed  bool
	evalTimeout time.Duration
	path        string
}

// newRegoEngine loads and compiles the policy file. Compilation happens here,
// at construction, so a malformed policy is rejected before the agent starts
// accepting exec requests rather than failing on the first operation.
func newRegoEngine(ctx context.Context, cfg Config) (*regoEngine, error) {
	src, err := os.ReadFile(cfg.File)
	if err != nil {
		return nil, fmt.Errorf("read policy file %s: %w", cfg.File, err)
	}

	query := cfg.Query
	if query == "" {
		query = DefaultQuery
	}
	evalTimeout := cfg.EvalTimeout
	if evalTimeout <= 0 {
		evalTimeout = DefaultEvalTimeout
	}

	// Compile from the in-memory module source (rather than rego.Load on a
	// path) so the same code path compiles example policy strings in tests
	// without touching the filesystem.
	const modName = "policy.rego"
	allow, err := rego.New(
		rego.Query(query),
		rego.Module(modName, string(src)),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile policy %s: %w", cfg.File, err)
	}

	e := &regoEngine{
		allow:       allow,
		failClosed:  cfg.FailClosed,
		evalTimeout: evalTimeout,
		path:        cfg.File,
	}

	// The reason rule is optional. Derive its query from the allow query
	// (a trailing ".allow" becomes ".reason") and prepare it separately; if it
	// doesn't compile or isn't present, denials simply carry no reason.
	if rq, ok := reasonQuery(query); ok {
		if prepared, rerr := rego.New(
			rego.Query(rq),
			rego.Module(modName, string(src)),
		).PrepareForEval(ctx); rerr == nil {
			e.reason = &prepared
		}
	}

	return e, nil
}

// reasonQuery derives the reason decision query from the allow query by
// swapping a trailing ".allow" for ".reason". Returns false if the query
// doesn't follow that convention.
func reasonQuery(allowQuery string) (string, bool) {
	if !strings.HasSuffix(allowQuery, ".allow") {
		return "", false
	}
	return strings.TrimSuffix(allowQuery, ".allow") + ".reason", true
}

func (e *regoEngine) Enabled() bool { return true }

func (e *regoEngine) Eval(ctx context.Context, input Input) (Decision, error) {
	raw, err := input.toRego()
	if err != nil {
		return e.onError(fmt.Errorf("marshal policy input: %w", err))
	}

	ectx, cancel := context.WithTimeout(ctx, e.evalTimeout)
	defer cancel()

	rs, err := e.allow.Eval(ectx, rego.EvalInput(raw))
	if err != nil {
		return e.onError(fmt.Errorf("eval allow: %w", err))
	}

	if rs.Allowed() {
		return Decision{Allow: true}, nil
	}

	return Decision{Allow: false, Reason: e.evalReason(ctx, raw)}, nil
}

// onError applies the configured failure mode. Fail-closed denies with a
// generic reason; fail-open allows. The error is returned for logging.
func (e *regoEngine) onError(err error) (Decision, error) {
	if e.failClosed {
		return Decision{Allow: false, Reason: failClosedReason}, err
	}
	return Decision{Allow: true}, err
}

// evalReason evaluates the optional reason rule. Returns "" when no reason
// rule is configured, the rule is undefined for this input, or evaluation
// fails — callers format an empty reason as a generic "policy denied".
func (e *regoEngine) evalReason(ctx context.Context, raw map[string]any) string {
	if e.reason == nil {
		return ""
	}
	ectx, cancel := context.WithTimeout(ctx, e.evalTimeout)
	defer cancel()

	rs, err := e.reason.Eval(ectx, rego.EvalInput(raw))
	if err != nil || len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return ""
	}
	if s, ok := rs[0].Expressions[0].Value.(string); ok {
		return s
	}
	return ""
}
