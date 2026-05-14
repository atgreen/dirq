// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// DB is the interface satisfied by *db.DB that the server package depends on.
// Extracting it allows HTTP handler tests to use an in-memory mock.
type DB interface {
	// Health
	Ping(ctx context.Context) error

	// Tokens
	ValidateToken(ctx context.Context, plaintext string) (db.Token, error)
	CreateToken(ctx context.Context, name, scope string) (string, error)
	ListTokens(ctx context.Context) ([]db.Token, error)
	DeleteToken(ctx context.Context, name string) error

	// Agents
	RegisterAgent(ctx context.Context, p db.RegisterAgentParams) (db.Agent, error)
	GetAgent(ctx context.Context, id string) (db.Agent, error)
	ListAgents(ctx context.Context, f db.ListAgentsFilter) ([]db.Agent, error)
	UpdateAgentHeartbeat(ctx context.Context, id string) error
	SetAgentOffline(ctx context.Context, id string) error
	SetAgentRole(ctx context.Context, id string, role string) error
	SetAgentParent(ctx context.Context, id string, parentID string) error
	UpdateAgentTags(ctx context.Context, id string, tags map[string]string) error
	MarkStaleAgentsOffline(ctx context.Context, threshold time.Duration) (int64, error)
	TouchAgentTree(ctx context.Context, rootID string) error
	MarkAgentTreeOffline(ctx context.Context, rootID string) (int64, error)

	// Facts
	GetFacts(ctx context.Context, agentID string) ([]db.Fact, error)
	UpsertFact(ctx context.Context, agentID, module string, data map[string]any) error

	// Queries
	CreateQuery(ctx context.Context, rawQuery, submittedBy string, targetCount int) (db.Query, error)
	UpdateQueryStatus(ctx context.Context, id, status string, successCount, errorCount, timeoutCount int) error
	ListQueries(ctx context.Context, limit int) ([]db.Query, error)

	// Exec log
	CreateExecLog(ctx context.Context, log db.ExecLog) (db.ExecLog, error)
	UpdateExecLog(ctx context.Context, id string, rc *int, success bool, errMsg string, finishedAt time.Time) error
	ListExecLogs(ctx context.Context, limit int) ([]db.ExecLog, error)
	ListExecLogsByAgent(ctx context.Context, agentID string, limit int) ([]db.ExecLog, error)
	ListExecLogsByJob(ctx context.Context, aapJobID string) ([]db.ExecLog, error)

	// Topology
	WithTopologyLock(ctx context.Context, fn func() error) error
	FindZoneLeader(ctx context.Context, agentID string) (db.Agent, error)
	FindShallowestParentWithRoom(ctx context.Context, maxChildren int) (db.Agent, error)
	FindFallbackParents(ctx context.Context, primaryParentID string, maxChildren int, count int) ([]db.Agent, error)
	CountAgentsByRole(ctx context.Context, role string) (int, error)
	CountOnlineZoneLeaders(ctx context.Context) (int, error)
	CountChildren(ctx context.Context, parentID string) (int, error)
	FindRelaysWithChildren(ctx context.Context) ([]db.Agent, error)
	FindImbalancedNodes(ctx context.Context, maxChildren int) (db.NodeLoad, db.NodeLoad, bool, error)
	FindChildOfParent(ctx context.Context, parentID string) (db.Agent, error)

	// Peers
	RegisterServerPeer(ctx context.Context, podID, addr string) error
}
