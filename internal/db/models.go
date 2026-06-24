// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"encoding/json"
	"strings"
	"time"
)

// EncodeAAPUsers serializes an aap_user allowlist into the comma-separated form
// stored in the api_tokens.aap_users column. Entries are trimmed and empties
// dropped; a nil/empty list encodes to "" (an unrestricted token).
func EncodeAAPUsers(users []string) string {
	cleaned := make([]string, 0, len(users))
	for _, u := range users {
		if u = strings.TrimSpace(u); u != "" {
			cleaned = append(cleaned, u)
		}
	}
	return strings.Join(cleaned, ",")
}

// ParseAAPUsers is the inverse of EncodeAAPUsers. An empty column yields nil
// (unrestricted).
func ParseAAPUsers(col string) []string {
	col = strings.TrimSpace(col)
	if col == "" {
		return nil
	}
	parts := strings.Split(col, ",")
	users := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			users = append(users, p)
		}
	}
	return users
}

// Agent represents a registered agent in the system.
type Agent struct {
	ID           string            `json:"id"`
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	OSVersion    string            `json:"os_version"`
	Arch         string            `json:"arch"`
	AgentVersion string            `json:"agent_version"`
	ListenAddr   string            `json:"listen_addr"`
	Role         string            `json:"role"`
	Capabilities []string          `json:"capabilities"`
	Tags         map[string]string `json:"tags"`
	ParentID     *string           `json:"parent_id,omitempty"`
	ServerPod    *string           `json:"server_pod,omitempty"`
	Online       bool              `json:"online"`
	ExecEnabled  bool              `json:"exec_enabled"`
	RegisteredAt time.Time         `json:"registered_at"`
	LastSeenAt   time.Time         `json:"last_seen_at"`
}

// RegisterAgentParams holds the parameters for registering a new agent.
type RegisterAgentParams struct {
	Hostname     string            `json:"hostname"`
	OS           string            `json:"os"`
	OSVersion    string            `json:"os_version"`
	Arch         string            `json:"arch"`
	AgentVersion string            `json:"agent_version"`
	ListenAddr   string            `json:"listen_addr"`
	Capabilities []string          `json:"capabilities"`
	Tags         map[string]string `json:"tags"`
	ExecEnabled  bool              `json:"exec_enabled"`
}

// ListAgentsFilter controls which agents are returned by ListAgents.
type ListAgentsFilter struct {
	Online   *bool   `json:"online,omitempty"`
	Role     string  `json:"role,omitempty"`
	ParentID string  `json:"parent_id,omitempty"`
	Tag      string  `json:"tag,omitempty"`    // key to match in tags JSONB
	TagValue string  `json:"tag_value,omitempty"` // value for the tag key
}

// Fact represents a cached fact collected from an agent.
type Fact struct {
	AgentID     string          `json:"agent_id"`
	Module      string          `json:"module"`
	Data        json.RawMessage `json:"data"`
	CollectedAt time.Time       `json:"collected_at"`
}

// FactRow is one row staged for a bulk fact upsert. Data is the
// already-marshalled JSON blob — the batcher marshals once at staging
// time so duplicate keys in a flush window overwrite cheaply.
type FactRow struct {
	AgentID     string
	Module      string
	Data        []byte
	CollectedAt time.Time
}

// Token represents an API token.
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	AAPUsers  []string   `json:"aap_users,omitempty"` // allowlist of aap_user values this token may assert; empty = unrestricted
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// Query represents a historical query.
type Query struct {
	ID           string     `json:"id"`
	RawQuery     string     `json:"raw_query"`
	SubmittedBy  *string    `json:"submitted_by,omitempty"`
	SubmittedAt  time.Time  `json:"submitted_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	Status       string     `json:"status"`
	TargetCount  int        `json:"target_count"`
	SuccessCount int        `json:"success_count"`
	ErrorCount   int        `json:"error_count"`
	TimeoutCount int        `json:"timeout_count"`
}

// ExecLog represents an entry in the execution audit log.
type ExecLog struct {
	ID             string     `json:"id"`
	RequestID      string     `json:"request_id"`
	AgentID        string     `json:"agent_id"`
	Hostname       string     `json:"hostname"`
	Operation      string     `json:"operation"`
	Command        *string    `json:"command,omitempty"`
	DestPath       *string    `json:"dest_path,omitempty"`
	SrcPath        *string    `json:"src_path,omitempty"`
	Become         bool       `json:"become"`
	BecomeUser     *string    `json:"become_user,omitempty"`
	RC             *int       `json:"rc,omitempty"`
	Success        *bool      `json:"success,omitempty"`
	Error          *string    `json:"error,omitempty"`
	AAPJobID       *string    `json:"aap_job_id,omitempty"`
	AAPJobTemplate *string    `json:"aap_job_template,omitempty"`
	AAPUser        *string    `json:"aap_user,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ServerPeer represents a server pod in a Podman deployment.
type ServerPeer struct {
	PodID        string    `json:"pod_id"`
	Addr         string    `json:"addr"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// NodeLoad represents an agent and its child count.
type NodeLoad struct {
	Agent      Agent
	ChildCount int
	Depth      int
}
