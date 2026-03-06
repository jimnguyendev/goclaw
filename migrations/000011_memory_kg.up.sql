-- Add temporal tracking columns to memory_chunks for decay scoring.
ALTER TABLE memory_chunks
    ADD COLUMN IF NOT EXISTS accessed_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS access_count   INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS is_evergreen   BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_mem_chunks_accessed
    ON memory_chunks(agent_id, accessed_at DESC NULLS LAST);

-- ─── Knowledge Graph ──────────────────────────────────────────────────────────

-- Canonical entity nodes.
CREATE TABLE memory_kg_nodes (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id       UUID        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    user_id        TEXT,
    canonical_name TEXT        NOT NULL,
    node_type      TEXT        NOT NULL DEFAULT 'entity',
    degree         INT         NOT NULL DEFAULT 0,  -- cached edge count for hub-capping
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_kg_nodes_unique
    ON memory_kg_nodes(agent_id, COALESCE(user_id, ''), canonical_name);
CREATE INDEX idx_kg_nodes_agent ON memory_kg_nodes(agent_id);

-- Alias → canonical node mapping.
CREATE TABLE memory_kg_aliases (
    agent_id  UUID NOT NULL,
    alias     TEXT NOT NULL,
    node_id   UUID NOT NULL REFERENCES memory_kg_nodes(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, alias)
);

-- Directed relationships between nodes (bi-temporal).
CREATE TABLE memory_kg_edges (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id    UUID        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    source_id   UUID        NOT NULL REFERENCES memory_kg_nodes(id) ON DELETE CASCADE,
    target_id   UUID        NOT NULL REFERENCES memory_kg_nodes(id) ON DELETE CASCADE,
    relation    TEXT        NOT NULL,
    weight      FLOAT       NOT NULL DEFAULT 1.0,
    -- T  timeline: when the fact was true in the world
    valid_from  TIMESTAMPTZ,
    valid_until TIMESTAMPTZ,
    -- T' timeline: when the system recorded/retracted the fact
    known_from  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    known_until TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_kg_edges_unique
    ON memory_kg_edges(agent_id, source_id, target_id, relation);
CREATE INDEX idx_kg_edges_source  ON memory_kg_edges(source_id);
CREATE INDEX idx_kg_edges_target  ON memory_kg_edges(target_id);
CREATE INDEX idx_kg_edges_agent   ON memory_kg_edges(agent_id);

-- Bidirectional entity ↔ chunk index.
CREATE TABLE memory_kg_chunk_mentions (
    chunk_id UUID NOT NULL REFERENCES memory_chunks(id) ON DELETE CASCADE,
    node_id  UUID NOT NULL REFERENCES memory_kg_nodes(id) ON DELETE CASCADE,
    PRIMARY KEY (chunk_id, node_id)
);
CREATE INDEX idx_kg_mentions_node ON memory_kg_chunk_mentions(node_id);

-- ─── Runtime-tunable search config (per-agent key-value) ─────────────────────
CREATE TABLE memory_search_config (
    agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    key      TEXT NOT NULL,
    value    TEXT NOT NULL,
    PRIMARY KEY (agent_id, key)
);
