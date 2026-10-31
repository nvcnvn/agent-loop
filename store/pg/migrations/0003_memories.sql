-- Long-term memory with pgvector embeddings. The vector column has no fixed
-- dimension so any embedding model fits; searches compare only vectors from
-- the same embed_model.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    memory_id   uuid        PRIMARY KEY,
    tenant_id   uuid        NOT NULL,
    scope       text        NOT NULL CHECK (scope IN ('tenant', 'workflow', 'session')),
    session_id  uuid,
    workflow    text,
    content     text        NOT NULL,
    embedding   vector      NOT NULL,
    embed_model text        NOT NULL,
    source      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (scope <> 'session' OR session_id IS NOT NULL),
    CHECK (scope <> 'workflow' OR workflow IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS memories_lookup_idx ON memories (tenant_id, embed_model, scope);
