// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Input is the stable, operation-specific document handed to Rego as `input`.
// It is deliberately decoupled from the generated protobuf messages: the wire
// shape is an implementation detail and a poor external contract, whereas this
// struct is the documented, versioned policy interface.
//
// Sensitive material is never included. Commands and paths are passed (they
// are the subject of policy), but file content, script bodies, stdin, and
// environment values are reduced to sizes, hashes, and key names only.
type Input struct {
	// Common fields — populated for every operation.
	Operation string            `json:"operation"`
	RequestID string            `json:"request_id"`
	AgentID   string            `json:"agent_id"`
	Hostname  string            `json:"hostname"`
	OS        string            `json:"os"`
	Tags      map[string]string `json:"tags,omitempty"`
	TimeUnix  int64             `json:"time_unix"`

	// Exec fields.
	Command         string   `json:"command,omitempty"`
	Script          bool     `json:"script"`
	ScriptName      string   `json:"script_name,omitempty"`
	ScriptSize      int      `json:"script_size,omitempty"`
	ScriptSHA256    string   `json:"script_sha256,omitempty"`
	StdinSize       int      `json:"stdin_size,omitempty"`
	EnvironmentKeys []string `json:"environment_keys,omitempty"`
	TimeoutSeconds  int32    `json:"timeout_seconds,omitempty"`
	AAPJobID        string   `json:"aap_job_id,omitempty"`
	AAPJobTemplate  string   `json:"aap_job_template,omitempty"`
	AAPUser         string   `json:"aap_user,omitempty"`

	// File / deploy fields.
	DestPath       string `json:"dest_path,omitempty"`
	SrcPath        string `json:"src_path,omitempty"`
	ContentSize    int    `json:"content_size,omitempty"`
	ContentSHA256  string `json:"content_sha256,omitempty"`
	Mode           uint32 `json:"mode,omitempty"`
	InstallCommand string `json:"install_command,omitempty"`

	// Privilege escalation — shared by exec, file, and deploy.
	Become       bool   `json:"become"`
	BecomeUser   string `json:"become_user,omitempty"`
	BecomeMethod string `json:"become_method,omitempty"`
}

// toRego converts the Input into the map shape OPA expects for `input`.
// OPA's evaluator accepts maps/slices/scalars but not arbitrary structs, so we
// round-trip through JSON. This also guarantees the policy sees exactly the
// documented field names and omitempty behavior.
func (in Input) toRego() (map[string]any, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SHA256Hex returns the lowercase hex SHA-256 of b. Used to populate the
// content/script hash fields without exposing the bytes themselves.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SortedKeys returns the keys of m in deterministic order, so the
// environment_keys field is stable across evaluations and reproducible in
// tests and audit logs.
func SortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
