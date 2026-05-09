-- Agent Loop initial PostgreSQL 18 schema sketch.
-- Assumptions:
-- - PostgreSQL 18 provides uuidv7().
-- - pgvector is available for embeddings.
-- - jsonb schemas use application-level JSON Schema validation first, database constraints second.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TYPE principal_type AS ENUM ('human', 'service', 'system', 'agent', 'external');
CREATE TYPE lifecycle_status AS ENUM ('draft', 'active', 'deprecated', 'disabled', 'archived', 'deleted');
CREATE TYPE session_status AS ENUM ('active', 'archived', 'deleted');
CREATE TYPE run_status AS ENUM ('queued', 'running', 'awaiting_approval', 'paused', 'canceling', 'canceled', 'succeeded', 'failed', 'timed_out', 'rejected', 'expired', 'superseded');
CREATE TYPE task_status AS ENUM ('pending', 'ready', 'running', 'blocked', 'completed', 'failed', 'retry_scheduled', 'canceled', 'skipped');
CREATE TYPE task_group_mode AS ENUM ('sequential', 'parallel', 'race', 'quorum', 'best_effort');
CREATE TYPE task_group_status AS ENUM ('pending', 'running', 'completed', 'partially_completed', 'failed', 'canceled');
CREATE TYPE failure_policy AS ENUM ('fail_fast', 'wait_all', 'quorum', 'best_effort');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'waiting', 'succeeded', 'failed', 'retry_scheduled', 'timed_out', 'canceled');
CREATE TYPE step_type AS ENUM ('human_message', 'agent_message', 'model_call', 'tool_call', 'approval_wait', 'subagent_spawn', 'snapshot', 'artifact', 'system');
CREATE TYPE event_visibility AS ENUM ('public', 'participant', 'internal', 'hidden_reasoning', 'audit');
CREATE TYPE tool_source AS ENUM ('native', 'mcp');
CREATE TYPE idempotency_hint AS ENUM ('unknown', 'idempotent', 'conditionally_idempotent', 'non_idempotent');
CREATE TYPE side_effect_level AS ENUM ('none', 'read_external', 'write_internal', 'write_external', 'dangerous');
CREATE TYPE concurrency_class AS ENUM ('serial', 'parallel_read', 'parallel_isolated_write', 'exclusive', 'external_side_effect');
CREATE TYPE invocation_status AS ENUM ('scheduled', 'running', 'succeeded', 'failed', 'retry_scheduled', 'timed_out', 'canceled');
CREATE TYPE approval_status AS ENUM ('requested', 'approved', 'rejected', 'expired', 'canceled');
CREATE TYPE artifact_kind AS ENUM ('file', 'json', 'report', 'code_patch', 'test_result', 'model_transcript');
CREATE TYPE memory_scope_type AS ENUM ('tenant', 'project', 'session', 'workflow', 'agent');
CREATE TYPE memory_status AS ENUM ('active', 'superseded', 'deleted');
CREATE TYPE budget_dimension AS ENUM ('prompt_tokens', 'completion_tokens', 'cached_tokens', 'total_tokens', 'cost_units', 'runtime_ms', 'model_calls', 'tool_calls', 'subagents', 'concurrency_slots');
CREATE TYPE budget_ledger_kind AS ENUM ('reserve', 'commit', 'release', 'adjust');

CREATE TABLE tenants (
    tenant_id uuid PRIMARY KEY DEFAULT uuidv7(),
    name text NOT NULL,
    status lifecycle_status NOT NULL DEFAULT 'active',
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE principals (
    principal_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    type principal_type NOT NULL,
    display_name text NOT NULL,
    auth_subject text,
    status lifecycle_status NOT NULL DEFAULT 'active',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, auth_subject)
);

CREATE TABLE sessions (
    session_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    created_by uuid NOT NULL REFERENCES principals(principal_id),
    title text,
    status session_status NOT NULL DEFAULT 'active',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    retention_policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,
    deleted_at timestamptz
);

CREATE INDEX sessions_tenant_status_created_idx ON sessions (tenant_id, status, created_at DESC);

CREATE TABLE session_participants (
    session_id uuid NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(principal_id),
    role text NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now(),
    left_at timestamptz,
    PRIMARY KEY (session_id, principal_id)
);

CREATE TABLE model_profiles (
    model_profile_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    provider text NOT NULL,
    model text NOT NULL,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    default_parameters jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider, model)
);

CREATE TABLE tool_policies (
    tool_policy_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    allowlist text[] NOT NULL DEFAULT '{}',
    denylist text[] NOT NULL DEFAULT '{}',
    approval_rules jsonb NOT NULL DEFAULT '{}'::jsonb,
    concurrency_limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    budget_limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE memory_policies (
    memory_policy_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    write_rules jsonb NOT NULL DEFAULT '{}'::jsonb,
    retrieval_rules jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE agent_definitions (
    agent_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    version integer NOT NULL DEFAULT 1,
    role text NOT NULL,
    instructions text NOT NULL,
    model_profile_id uuid NOT NULL REFERENCES model_profiles(model_profile_id),
    tool_policy_id uuid REFERENCES tool_policies(tool_policy_id),
    memory_policy_id uuid REFERENCES memory_policies(memory_policy_id),
    default_limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    allowed_child_agents uuid[] NOT NULL DEFAULT '{}',
    status lifecycle_status NOT NULL DEFAULT 'draft',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name, version)
);

CREATE TABLE workflow_templates (
    workflow_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    version integer NOT NULL DEFAULT 1,
    definition jsonb NOT NULL,
    input_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    output_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'draft',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name, version)
);

CREATE TABLE runs (
    run_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    workflow_id uuid REFERENCES workflow_templates(workflow_id),
    parent_run_id uuid REFERENCES runs(run_id),
    root_task_id uuid,
    status run_status NOT NULL DEFAULT 'queued',
    limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    usage_totals jsonb NOT NULL DEFAULT '{}'::jsonb,
    replay_mode boolean NOT NULL DEFAULT false,
    idempotency_key text,
    failure_reason jsonb,
    queued_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    ended_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX runs_session_created_idx ON runs (session_id, queued_at DESC);
CREATE INDEX runs_tenant_status_idx ON runs (tenant_id, status, queued_at DESC);

CREATE TABLE tasks (
    task_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    parent_task_id uuid REFERENCES tasks(task_id),
    agent_id uuid NOT NULL REFERENCES agent_definitions(agent_id),
    status task_status NOT NULL DEFAULT 'pending',
    depth integer NOT NULL DEFAULT 0 CHECK (depth >= 0),
    attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    input jsonb NOT NULL DEFAULT '{}'::jsonb,
    output jsonb,
    error jsonb,
    deadline_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    ended_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE runs ADD CONSTRAINT runs_root_task_fk FOREIGN KEY (root_task_id) REFERENCES tasks(task_id);
CREATE INDEX tasks_run_status_idx ON tasks (run_id, status, created_at);
CREATE INDEX tasks_parent_idx ON tasks (parent_task_id, created_at);

CREATE TABLE task_groups (
    task_group_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    parent_task_id uuid REFERENCES tasks(task_id),
    mode task_group_mode NOT NULL,
    failure_policy failure_policy NOT NULL DEFAULT 'fail_fast',
    quorum_count integer CHECK (quorum_count IS NULL OR quorum_count > 0),
    status task_group_status NOT NULL DEFAULT 'pending',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_group_members (
    task_group_id uuid NOT NULL REFERENCES task_groups(task_group_id) ON DELETE CASCADE,
    task_id uuid NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    required boolean NOT NULL DEFAULT true,
    PRIMARY KEY (task_group_id, task_id),
    UNIQUE (task_group_id, ordinal)
);

CREATE TABLE steps (
    step_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    task_id uuid REFERENCES tasks(task_id),
    parent_step_id uuid REFERENCES steps(step_id),
    step_type step_type NOT NULL,
    status step_status NOT NULL DEFAULT 'pending',
    attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    idempotency_key text,
    input jsonb NOT NULL DEFAULT '{}'::jsonb,
    output jsonb,
    error jsonb,
    started_at timestamptz,
    ended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, idempotency_key)
);

CREATE INDEX steps_run_task_idx ON steps (run_id, task_id, created_at);
CREATE INDEX steps_status_idx ON steps (tenant_id, status, created_at);

CREATE TABLE run_events (
    event_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    run_id uuid REFERENCES runs(run_id) ON DELETE CASCADE,
    sequence bigint NOT NULL,
    session_sequence bigint NOT NULL,
    event_type text NOT NULL,
    visibility event_visibility NOT NULL DEFAULT 'internal',
    actor_principal_id uuid REFERENCES principals(principal_id),
    task_id uuid REFERENCES tasks(task_id),
    step_id uuid REFERENCES steps(step_id),
    parent_event_id uuid REFERENCES run_events(event_id),
    correlation_id uuid NOT NULL DEFAULT uuidv7(),
    causation_id uuid,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    hidden_payload jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    recorded_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, sequence),
    UNIQUE (session_id, session_sequence),
    CHECK (visibility = 'hidden_reasoning' OR hidden_payload IS NULL)
);

CREATE INDEX run_events_run_seq_idx ON run_events (run_id, sequence);
CREATE INDEX run_events_session_seq_idx ON run_events (session_id, session_sequence);
CREATE INDEX run_events_public_idx ON run_events (run_id, sequence) WHERE visibility IN ('public', 'participant');
CREATE INDEX run_events_payload_gin_idx ON run_events USING gin (payload);

CREATE TABLE messages (
    message_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    run_id uuid REFERENCES runs(run_id),
    event_id uuid NOT NULL REFERENCES run_events(event_id),
    role text NOT NULL,
    visibility event_visibility NOT NULL DEFAULT 'participant',
    content jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX messages_session_created_idx ON messages (session_id, created_at);

CREATE TABLE tool_definitions (
    tool_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    version text NOT NULL,
    source tool_source NOT NULL,
    description text NOT NULL DEFAULT '',
    input_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    output_schema jsonb NOT NULL DEFAULT '{}'::jsonb,
    idempotency idempotency_hint NOT NULL DEFAULT 'unknown',
    side_effect side_effect_level NOT NULL DEFAULT 'none',
    concurrency concurrency_class NOT NULL DEFAULT 'serial',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name, version)
);

CREATE TABLE mcp_servers (
    mcp_server_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    name text NOT NULL,
    endpoint text NOT NULL,
    auth_ref text,
    connection_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    status lifecycle_status NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE tool_invocations (
    tool_invocation_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    task_id uuid REFERENCES tasks(task_id),
    step_id uuid REFERENCES steps(step_id),
    tool_id uuid NOT NULL REFERENCES tool_definitions(tool_id),
    status invocation_status NOT NULL DEFAULT 'scheduled',
    idempotency_key text,
    input jsonb NOT NULL,
    output jsonb,
    error jsonb,
    started_at timestamptz,
    ended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, idempotency_key)
);

CREATE INDEX tool_invocations_run_idx ON tool_invocations (run_id, created_at);
CREATE INDEX tool_invocations_status_idx ON tool_invocations (tenant_id, status, created_at);

CREATE TABLE approvals (
    approval_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    step_id uuid REFERENCES steps(step_id),
    requested_by uuid REFERENCES principals(principal_id),
    resolved_by uuid REFERENCES principals(principal_id),
    status approval_status NOT NULL DEFAULT 'requested',
    reason text NOT NULL,
    request_payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    resolution_payload jsonb,
    requested_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    resolved_at timestamptz
);

CREATE INDEX approvals_pending_idx ON approvals (tenant_id, expires_at, requested_at) WHERE status = 'requested';

CREATE TABLE snapshots (
    snapshot_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    run_id uuid NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    created_by_step_id uuid REFERENCES steps(step_id),
    covers_sequence_from bigint NOT NULL,
    covers_sequence_to bigint NOT NULL,
    summary jsonb NOT NULL,
    model_input jsonb,
    model_output jsonb,
    token_usage jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (covers_sequence_from <= covers_sequence_to)
);

CREATE INDEX snapshots_run_range_idx ON snapshots (run_id, covers_sequence_to DESC);

CREATE TABLE artifacts (
    artifact_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    session_id uuid NOT NULL REFERENCES sessions(session_id),
    run_id uuid REFERENCES runs(run_id),
    task_id uuid REFERENCES tasks(task_id),
    step_id uuid REFERENCES steps(step_id),
    kind artifact_kind NOT NULL,
    name text NOT NULL,
    mime_type text NOT NULL DEFAULT 'application/octet-stream',
    version integer NOT NULL DEFAULT 1,
    storage_uri text,
    content jsonb,
    size_bytes bigint,
    checksum text,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, session_id, name, version),
    CHECK (storage_uri IS NOT NULL OR content IS NOT NULL)
);

CREATE INDEX artifacts_run_idx ON artifacts (run_id, created_at);

CREATE TABLE memory_items (
    memory_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    scope_type memory_scope_type NOT NULL,
    scope_id uuid,
    status memory_status NOT NULL DEFAULT 'active',
    content text NOT NULL,
    content_hash text NOT NULL,
    embedding vector(1536),
    provenance jsonb NOT NULL DEFAULT '{}'::jsonb,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_from_event_id uuid REFERENCES run_events(event_id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, scope_type, scope_id, content_hash)
);

CREATE INDEX memory_items_scope_idx ON memory_items (tenant_id, scope_type, scope_id, status, created_at DESC);
CREATE INDEX memory_items_embedding_idx ON memory_items USING hnsw (embedding vector_cosine_ops);

CREATE TABLE budget_ledger (
    budget_ledger_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    run_id uuid REFERENCES runs(run_id) ON DELETE CASCADE,
    task_id uuid REFERENCES tasks(task_id),
    step_id uuid REFERENCES steps(step_id),
    dimension budget_dimension NOT NULL,
    kind budget_ledger_kind NOT NULL,
    amount numeric NOT NULL CHECK (amount >= 0),
    reason text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX budget_ledger_run_idx ON budget_ledger (run_id, created_at);
CREATE INDEX budget_ledger_tenant_dimension_idx ON budget_ledger (tenant_id, dimension, created_at);

CREATE TABLE audit_log (
    audit_id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id uuid NOT NULL REFERENCES tenants(tenant_id),
    actor_principal_id uuid REFERENCES principals(principal_id),
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    correlation_id uuid NOT NULL DEFAULT uuidv7(),
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_time_idx ON audit_log (tenant_id, occurred_at DESC);
CREATE INDEX audit_log_resource_idx ON audit_log (resource_type, resource_id, occurred_at DESC);

-- Sequence allocation should be done transactionally by the event store.
-- One option is a per-run counter row locked with SELECT ... FOR UPDATE.
CREATE TABLE event_counters (
    scope_type text NOT NULL,
    scope_id uuid NOT NULL,
    next_sequence bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (scope_type, scope_id)
);
