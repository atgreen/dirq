// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/atgreen/dirq/internal/agent/policy"
)

// evalPolicy fills in the host-identity fields of in, evaluates the policy
// engine, and logs the decision. It returns the resulting Decision; the caller
// denies the operation when Allow is false. When no policy is configured this
// is a cheap allow with no logging.
//
// Callers should build only the operation-specific fields of in and let this
// method stamp agent_id / hostname / os / tags / time_unix so every operation
// reports identity consistently.
func (a *Agent) evalPolicy(ctx context.Context, in policy.Input) policy.Decision {
	if a.policyEngine == nil || !a.policyEngine.Enabled() {
		return policy.Decision{Allow: true}
	}

	in.AgentID = a.agentID
	in.Hostname = a.hostname
	in.OS = runtime.GOOS
	if len(a.cfg.Tags) > 0 {
		in.Tags = a.cfg.Tags
	}
	in.TimeUnix = time.Now().Unix()

	dec, err := a.policyEngine.Eval(ctx, in)

	switch {
	case err != nil:
		// Evaluation error: the Decision already reflects fail-open/closed.
		a.log.Error("policy evaluation error",
			slog.String("request_id", in.RequestID),
			slog.String("operation", in.Operation),
			slog.Bool("allow", dec.Allow),
			slog.String("policy_file", a.cfg.PolicyFile),
			slog.String("error", err.Error()),
		)
	case !dec.Allow:
		// Denials are the audit-worthy event — log at Warn so break-glass and
		// blocked operations are easy to detect.
		a.log.Warn("policy denied operation",
			slog.String("request_id", in.RequestID),
			slog.String("operation", in.Operation),
			slog.String("reason", dec.Reason),
			slog.String("policy_file", a.cfg.PolicyFile),
		)
	default:
		a.log.Debug("policy allowed operation",
			slog.String("request_id", in.RequestID),
			slog.String("operation", in.Operation),
		)
	}

	return dec
}

// policyDeniedError formats a denial reason into the wire error string. The
// "policy denied:" prefix is a stable contract the CLI and broadcast paths key
// on; an empty reason collapses to the bare prefix.
func policyDeniedError(reason string) string {
	if reason == "" {
		return "policy denied"
	}
	return "policy denied: " + reason
}
