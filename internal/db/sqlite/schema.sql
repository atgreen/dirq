-- SPDX-License-Identifier: MIT
-- Copyright (c) 2026 Anthony Green <green@moxielogic.com>

-- DirQ SQLite Schema (idempotent — safe to run repeatedly)

-- ─────────────────────────────────────────────────────────
-- Agents (host registry)
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS agents (
    id            TEXT PRIMARY KEY,
    hostname      TEXT NOT NULL,
    os            TEXT NOT NULL,
    os_version    TEXT NOT NULL DEFAULT '',
    arch          TEXT NOT NULL DEFAULT 'amd64',
    agent_version TEXT NOT NULL DEFAULT '',
    listen_addr   TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'leaf',
    capabilities  TEXT NOT NULL DEFAULT '[]',
    tags          TEXT NOT NULL DEFAULT '{}',
    parent_id     TEXT REFERENCES agents(id) ON DELETE SET NULL,
    server_pod    TEXT,
    online        INTEGER NOT NULL DEFAULT 1,
    exec_enabled  INTEGER NOT NULL DEFAULT 0,
    registered_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_hostname ON agents(hostname);
CREATE INDEX IF NOT EXISTS idx_agents_online ON agents(online);
CREATE INDEX IF NOT EXISTS idx_agents_role ON agents(role);
CREATE INDEX IF NOT EXISTS idx_agents_parent ON agents(parent_id);

-- ─────────────────────────────────────────────────────────
-- Fact cache
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS facts (
    agent_id     TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    module       TEXT NOT NULL,
    data         TEXT NOT NULL,
    collected_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (agent_id, module)
);

CREATE INDEX IF NOT EXISTS idx_facts_module ON facts(module);
CREATE INDEX IF NOT EXISTS idx_facts_collected ON facts(collected_at);

-- ─────────────────────────────────────────────────────────
-- Query history
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS queries (
    id            TEXT PRIMARY KEY,
    raw_query     TEXT NOT NULL,
    submitted_by  TEXT,
    submitted_at  TEXT NOT NULL DEFAULT (datetime('now')),
    completed_at  TEXT,
    status        TEXT NOT NULL DEFAULT 'running',
    target_count  INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    error_count   INTEGER NOT NULL DEFAULT 0,
    timeout_count INTEGER NOT NULL DEFAULT 0
);

-- ─────────────────────────────────────────────────────────
-- API tokens
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL DEFAULT '',
    token_hash   TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'admin',
    aap_users    TEXT NOT NULL DEFAULT '',  -- comma-separated allowlist of aap_user values this token may assert; empty = unrestricted
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_used    TEXT
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_prefix ON api_tokens (token_prefix);

-- ─────────────────────────────────────────────────────────
-- Server peers (for Podman dev — pod discovery)
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS server_peers (
    pod_id      TEXT PRIMARY KEY,
    addr        TEXT NOT NULL,
    registered_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ─────────────────────────────────────────────────────────
-- Fact TTL configuration
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS fact_ttl (
    module      TEXT PRIMARY KEY,
    ttl_seconds INTEGER NOT NULL DEFAULT 900
);

INSERT OR IGNORE INTO fact_ttl (module, ttl_seconds) VALUES ('_default', 900);
INSERT OR IGNORE INTO fact_ttl (module, ttl_seconds) VALUES ('disk', 300);
INSERT OR IGNORE INTO fact_ttl (module, ttl_seconds) VALUES ('cpu', 3600);
INSERT OR IGNORE INTO fact_ttl (module, ttl_seconds) VALUES ('memory', 300);

-- ─────────────────────────────────────────────────────────
-- Execution audit log
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS exec_log (
    id            TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL,
    operation     TEXT NOT NULL,
    command       TEXT,
    dest_path     TEXT,
    src_path      TEXT,
    become        INTEGER NOT NULL DEFAULT 0,
    become_user   TEXT,
    rc            INTEGER,
    success       INTEGER,
    error         TEXT,
    aap_job_id    TEXT,
    aap_job_template TEXT,
    aap_user      TEXT,
    started_at    TEXT,
    finished_at   TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_exec_log_agent ON exec_log(agent_id);
CREATE INDEX IF NOT EXISTS idx_exec_log_aap_job ON exec_log(aap_job_id);
CREATE INDEX IF NOT EXISTS idx_exec_log_created ON exec_log(created_at);
