-- DirQ PostgreSQL Schema

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ─────────────────────────────────────────────────────────
-- Agents (host registry)
-- ─────────────────────────────────────────────────────────

CREATE TABLE agents (
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
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_agents_hostname ON agents(hostname);
CREATE INDEX idx_agents_online ON agents(online);
CREATE INDEX idx_agents_role ON agents(role);
CREATE INDEX idx_agents_parent ON agents(parent_id);
CREATE INDEX idx_agents_tags ON agents USING gin(tags);

-- ─────────────────────────────────────────────────────────
-- Fact cache
-- ─────────────────────────────────────────────────────────

CREATE TABLE facts (
    agent_id     TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    module       TEXT NOT NULL,           -- 'disk', 'cpu', 'memory', etc.
    data         JSONB NOT NULL,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, module)
);

CREATE INDEX idx_facts_module ON facts(module);
CREATE INDEX idx_facts_collected ON facts(collected_at);

-- ─────────────────────────────────────────────────────────
-- Query history
-- ─────────────────────────────────────────────────────────

CREATE TABLE queries (
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

CREATE TABLE api_tokens (
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

CREATE TABLE server_peers (
    pod_id      TEXT PRIMARY KEY,
    addr        TEXT NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────
-- Fact TTL configuration
-- ─────────────────────────────────────────────────────────

CREATE TABLE fact_ttl (
    module      TEXT PRIMARY KEY,         -- module name or '_default'
    ttl_seconds INTEGER NOT NULL DEFAULT 900  -- 15 minutes default
);

INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('_default', 900);
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('disk', 300);
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('cpu', 3600);
INSERT INTO fact_ttl (module, ttl_seconds) VALUES ('memory', 300);
