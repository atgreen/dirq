// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"
	"log/slog"
	"time"
)

// Leader is a handle for the active leader-election worker. A process
// holding the lock reports IsLeader() == true; Run() blocks while it
// continuously tries to acquire (and hold) the singleton leader lock.
type Leader interface {
	IsLeader() bool
	Run(ctx context.Context)
}

// DB is the interface that both postgres and sqlite backends implement.
type DB interface {
	// Health
	Ping(ctx context.Context) error
	Close()
	RunMigrations(ctx context.Context) error
	Kind() string // "postgres" or "sqlite"

	// NewLeader constructs a leader-election worker for this backend.
	// Postgres uses an advisory lock; SQLite (single-instance) always
	// reports leadership.
	NewLeader(log *slog.Logger) Leader

	// Tokens
	ValidateToken(ctx context.Context, plaintext string) (Token, error)
	CreateToken(ctx context.Context, name, scope string, aapUsers []string) (string, error)
	ListTokens(ctx context.Context) ([]Token, error)
	DeleteToken(ctx context.Context, name string) error

	// Agents
	RegisterAgent(ctx context.Context, p RegisterAgentParams) (Agent, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	GetAgentByHostname(ctx context.Context, hostname string) (Agent, error)
	ListAgents(ctx context.Context, f ListAgentsFilter) ([]Agent, error)
	UpdateAgentHeartbeat(ctx context.Context, id string) error
	SetAgentOffline(ctx context.Context, id string) error
	SetAgentRole(ctx context.Context, id string, role string) error
	SetAgentParent(ctx context.Context, id string, parentID string) error
	UpdateAgentTags(ctx context.Context, id string, tags map[string]string) error
	DeleteAgent(ctx context.Context, id string) error
	MarkStaleAgentsOffline(ctx context.Context, threshold time.Duration) (int64, error)
	TouchAgentTree(ctx context.Context, rootID string) error
	MarkAgentTreeOffline(ctx context.Context, rootID string) (int64, error)

	// Facts
	GetFacts(ctx context.Context, agentID string) ([]Fact, error)
	GetAllFacts(ctx context.Context) ([]Fact, error)
	GetFactsByModule(ctx context.Context, module string) ([]Fact, error)
	UpsertFact(ctx context.Context, agentID, module string, data map[string]any) error
	BulkUpsertFacts(ctx context.Context, rows []FactRow) error
	GetFactTTL(ctx context.Context, module string) (int, error)

	// Queries
	CreateQuery(ctx context.Context, rawQuery, submittedBy string, targetCount int) (Query, error)
	UpdateQueryStatus(ctx context.Context, id, status string, successCount, errorCount, timeoutCount int) error
	ListQueries(ctx context.Context, limit int) ([]Query, error)

	// Exec log
	CreateExecLog(ctx context.Context, log ExecLog) (ExecLog, error)
	UpdateExecLog(ctx context.Context, id string, rc *int, success bool, errMsg string, finishedAt time.Time) error
	ListExecLogs(ctx context.Context, limit int) ([]ExecLog, error)
	ListExecLogsByAgent(ctx context.Context, agentID string, limit int) ([]ExecLog, error)
	ListExecLogsByJob(ctx context.Context, aapJobID string) ([]ExecLog, error)

	// Topology
	WithTopologyLock(ctx context.Context, fn func() error) error
	FindZoneLeader(ctx context.Context, agentID string) (Agent, error)
	FindShallowestParentWithRoom(ctx context.Context, maxChildren int) (Agent, error)
	FindFallbackParents(ctx context.Context, primaryParentID string, maxChildren int, count int) ([]Agent, error)
	CountAgentsByRole(ctx context.Context, role string) (int, error)
	CountOnlineZoneLeaders(ctx context.Context) (int, error)
	CountChildren(ctx context.Context, parentID string) (int, error)
	FindRelaysWithChildren(ctx context.Context) ([]Agent, error)
	FindImbalancedNodes(ctx context.Context, maxChildren int) (NodeLoad, NodeLoad, bool, error)
	FindChildOfParent(ctx context.Context, parentID string) (Agent, error)
	FindParentWithRoom(ctx context.Context, role string, maxChildren int) (Agent, error)

	// Peers
	RegisterServerPeer(ctx context.Context, podID, addr string) error
	ListServerPeers(ctx context.Context) ([]ServerPeer, error)
	RemoveServerPeer(ctx context.Context, podID string) error
}
