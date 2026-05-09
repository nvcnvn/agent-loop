-- Minimal Postgres schema for the agent-loop pg driver.
--
-- This is the subset of docs/02-postgres-schema.sql that the current
-- runtime actually writes to. It is intentionally narrower than the full
-- design schema so the driver stays easy to evolve.
--
-- Apply with:
--   psql "$DATABASE_URL" -f store/pg/schema.sql

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS sessions (
    session_id   uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL,
    created_by   uuid        NOT NULL,
    title        text        NOT NULL DEFAULT '',
    status       text        NOT NULL DEFAULT 'active',
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_tenant_idx ON sessions (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS runs (
    run_id          uuid        PRIMARY KEY,
    tenant_id       uuid        NOT NULL,
    session_id      uuid        NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    parent_run_id   uuid,
    status          text        NOT NULL,
    limits          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    usage_totals    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    failure_reason  text        NOT NULL DEFAULT '',
    idempotency_key text,
    workflow        jsonb,
    input           jsonb,
    output          jsonb,
    queued_at       timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    ended_at        timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS runs_session_idx ON runs (session_id, queued_at DESC);

CREATE TABLE IF NOT EXISTS tasks (
    task_id        uuid        PRIMARY KEY,
    tenant_id      uuid        NOT NULL,
    run_id         uuid        NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    parent_task_id uuid,
    agent_name     text        NOT NULL,
    status         text        NOT NULL,
    depth          integer     NOT NULL DEFAULT 0,
    attempt        integer     NOT NULL DEFAULT 1,
    input          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    output         jsonb,
    error          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tasks_run_idx ON tasks (run_id, created_at);

-- Per-scope monotonic sequence counters. We use INSERT ... ON CONFLICT
-- DO UPDATE ... RETURNING so allocation is a single round-trip and is
-- atomic under concurrent writers.
CREATE TABLE IF NOT EXISTS event_counters (
    scope_type    text   NOT NULL,
    scope_id      uuid   NOT NULL,
    next_sequence bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (scope_type, scope_id)
);

CREATE TABLE IF NOT EXISTS run_events (
    event_id          uuid        PRIMARY KEY,
    tenant_id         uuid        NOT NULL,
    session_id        uuid        NOT NULL,
    run_id            uuid,
    sequence          bigint      NOT NULL DEFAULT 0,
    session_sequence  bigint      NOT NULL DEFAULT 0,
    event_type        text        NOT NULL,
    visibility        text        NOT NULL DEFAULT 'internal',
    actor_id          uuid,
    task_id           uuid,
    step_id           uuid,
    parent_event_id   uuid,
    correlation_id    uuid        NOT NULL,
    causation_id      uuid,
    payload           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    hidden_payload    jsonb,
    occurred_at       timestamptz NOT NULL DEFAULT now(),
    recorded_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS run_events_run_seq_idx
    ON run_events (run_id, sequence) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS run_events_session_seq_idx
    ON run_events (session_id, session_sequence) WHERE session_sequence > 0;
CREATE INDEX IF NOT EXISTS run_events_session_recorded_idx
    ON run_events (session_id, recorded_at);

CREATE TABLE IF NOT EXISTS messages (
    message_id  uuid        PRIMARY KEY,
    session_id  uuid        NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    run_id      uuid,
    event_id    uuid,
    role        text        NOT NULL,
    visibility  text        NOT NULL DEFAULT 'participant',
    content     text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS messages_session_idx ON messages (session_id, created_at);

CREATE TABLE IF NOT EXISTS approvals (
    approval_id  uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL,
    run_id       uuid,
    step_id      uuid,
    reason       text        NOT NULL DEFAULT '',
    status       text        NOT NULL DEFAULT 'requested',
    request      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    resolution   jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz
);

CREATE TABLE IF NOT EXISTS artifacts (
    artifact_id uuid        PRIMARY KEY,
    tenant_id   uuid        NOT NULL,
    session_id  uuid        NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    run_id      uuid,
    task_id     uuid,
    kind        text        NOT NULL DEFAULT 'json',
    name        text        NOT NULL,
    mime_type   text        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes  bigint      NOT NULL DEFAULT 0,
    content     bytea,
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS artifacts_run_idx ON artifacts (run_id, created_at);
