-- SPDX-License-Identifier: MIT
-- Copyright (c) 2026 Anthony Green <green@moxielogic.com>

-- DirQ PostgreSQL Schema (idempotent — safe to run repeatedly)

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ─────────────────────────────────────────────────────────
-- Agents (host registry)
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS agents (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    hostname      TEXT NOT NULL,
    os            TEXT NOT NULL,          -- 'linux' or 'windows'
    os_version    TEXT NOT NULL DEFAULT '',
    arch          TEXT NOT NULL DEFAULT 'amd64',
    agent_version TEXT NOT NULL DEFAULT '',
    listen_addr   TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'leaf',  -- 'leaf', 'relay', 'zone_leader'
    capabilities  TEXT[] NOT NULL DEFAULT '{}',
    tags          JSONB NOT NULL DEFAULT '{}',
    parent_id     TEXT REFERENCES agents(id) ON DELETE SET NULL,
    server_pod    TEXT,                  -- which server pod owns this agent's stream
    online        BOOLEAN NOT NULL DEFAULT true,
    exec_enabled  BOOLEAN NOT NULL DEFAULT false,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_hostname ON agents(hostname);
CREATE INDEX IF NOT EXISTS idx_agents_online ON agents(online);
CREATE INDEX IF NOT EXISTS idx_agents_role ON agents(role);
CREATE INDEX IF NOT EXISTS idx_agents_parent ON agents(parent_id);
CREATE INDEX IF NOT EXISTS idx_agents_tags ON agents USING gin(tags);

-- Migration: add exec_enabled if upgrading from an older schema.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS exec_enabled BOOLEAN NOT NULL DEFAULT false;

-- ─────────────────────────────────────────────────────────
-- Fact cache
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS facts (
    agent_id     TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    module       TEXT NOT NULL,           -- 'disk', 'cpu', 'memory', etc.
    data         JSONB NOT NULL,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, module)
);

CREATE INDEX IF NOT EXISTS idx_facts_module ON facts(module);
CREATE INDEX IF NOT EXISTS idx_facts_collected ON facts(collected_at);

-- ─────────────────────────────────────────────────────────
-- Query history
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS queries (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    raw_query     TEXT NOT NULL,
    submitted_by  TEXT,                   -- API token name
    submitted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ,
    status        TEXT NOT NULL DEFAULT 'running', -- 'running', 'completed', 'failed'
    target_count  INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    error_count   INTEGER NOT NULL DEFAULT 0,
    timeout_count INTEGER NOT NULL DEFAULT 0
);

-- ─────────────────────────────────────────────────────────
-- API tokens
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS api_tokens (
    id         TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name       TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL,            -- bcrypt hash
    scope      TEXT NOT NULL DEFAULT 'admin', -- 'admin' or 'readonly'
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used  TIMESTAMPTZ
);

-- ─────────────────────────────────────────────────────────
-- Server peers (for Podman dev — pod discovery)
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS server_peers (
    pod_id      TEXT PRIMARY KEY,
    addr        TEXT NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────
-- Fact TTL configuration
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS fact_ttl (
    module      TEXT PRIMARY KEY,         -- module name or '_default'
    ttl_seconds INTEGER NOT NULL DEFAULT 900  -- 15 minutes default
);

INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('_default', 900) ON CONFLICT DO NOTHING;
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('disk', 300) ON CONFLICT DO NOTHING;
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('cpu', 3600) ON CONFLICT DO NOTHING;
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('memory', 300) ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────
-- Execution audit log
-- ─────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS exec_log (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    request_id    TEXT NOT NULL,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL,
    operation     TEXT NOT NULL,  -- 'exec_command', 'put_file', 'fetch_file'
    command       TEXT,           -- for exec_command
    dest_path     TEXT,           -- for put_file
    src_path      TEXT,           -- for fetch_file
    become        BOOLEAN NOT NULL DEFAULT false,
    become_user   TEXT,
    rc            INTEGER,
    success       BOOLEAN,
    error         TEXT,
    aap_job_id    TEXT,
    aap_job_template TEXT,
    aap_user      TEXT,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_exec_log_agent ON exec_log(agent_id);
CREATE INDEX IF NOT EXISTS idx_exec_log_aap_job ON exec_log(aap_job_id);
CREATE INDEX IF NOT EXISTS idx_exec_log_created ON exec_log(created_at);
