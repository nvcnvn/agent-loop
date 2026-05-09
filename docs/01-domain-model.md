# Domain Model and State Machines

This document turns `requirement.md` into a first implementation domain model. The recommended initial scope is multi-tenant capable data structures with a single-tenant deployment mode enabled by configuration. This avoids rewriting persistence later while keeping the first runtime simple.

## Core Entity Map

### Tenant and Principal

`tenant` is the isolation boundary for sessions, tools, artifacts, memory, budgets, and audit records.

`principal` is an authenticated actor. It can represent a human user, service account, system worker, agent runtime, or external integration.

Important fields:
- `tenant_id`
- `principal_id`
- `principal_type`: `human`, `service`, `system`, `agent`, `external`
- `display_name`
- `auth_subject`
- `status`

### Session

A `session` is a long-lived collaboration container. It owns ordered history, runs, artifacts, memory writes, and participants.

Important fields:
- `session_id`
- `tenant_id`
- `created_by`
- `title`
- `status`
- `metadata`
- `retention_policy`

Session state:

```text
active -> archived -> deleted
active -> deleted
```

Rules:
- Only `active` sessions can accept human messages or start runs.
- `archived` sessions are read-only but replayable.
- `deleted` should be a tombstone state first; physical deletion follows retention policy.

### Agent Definition

An `agent_definition` is a configured reasoning unit. It is not itself an execution instance.

Important fields:
- `agent_id`
- `tenant_id`
- `name`
- `role`
- `instructions`
- `model_ref`
- `tool_policy_id`
- `memory_policy_id`
- `default_limits`
- `allowed_child_agents`
- `status`

Agent definition state:

```text
draft -> active -> deprecated -> disabled
draft -> disabled
active -> disabled
deprecated -> disabled
```

Rules:
- A run should pin the exact agent definition version used.
- Deprecated agents can resume old runs but cannot start new runs unless explicitly allowed.

### Workflow Template

A `workflow_template` is a predefined graph or staged plan. Dynamic delegation can still happen inside the limits declared here.

Important fields:
- `workflow_id`
- `tenant_id`
- `name`
- `version`
- `definition`
- `input_schema`
- `output_schema`
- `status`

Workflow state:

```text
draft -> active -> deprecated -> disabled
active -> deprecated
```

Definition must include:
- agent roles
- allowed tools
- allowed child agents
- retry policy
- timeout and deadline
- stop conditions
- human approval gates
- input and output schema
- explicit concurrent groups when concurrency is intended

### Run

A `run` is one execution attempt inside a session. It may contain multiple tasks, sub-agent runs, model calls, tool calls, approval waits, and snapshots.

Important fields:
- `run_id`
- `session_id`
- `tenant_id`
- `root_task_id`
- `workflow_id`
- `parent_run_id`
- `status`
- `limits`
- `usage_totals`
- `failure_reason`
- `started_at`, `ended_at`

Run state:

```text
queued -> running -> succeeded
queued -> canceled
running -> awaiting_approval -> running
running -> paused -> running
running -> canceling -> canceled
running -> failed
running -> timed_out
running -> succeeded
awaiting_approval -> rejected
awaiting_approval -> expired
paused -> canceling
failed -> queued        # retry as same run attempt only when replay policy allows
failed -> superseded    # new run attempt replaces it
```

Rules:
- `run.status` is a projection of the event stream, not the source of truth.
- A run is resumable when its latest durable event is not terminal and all in-flight work can be reconciled.
- Sub-agent execution should be represented as child tasks and optionally child runs when independent replay boundaries are needed.

### Task

A `task` is a delegated work item assigned to an agent definition or runtime agent instance.

Important fields:
- `task_id`
- `run_id`
- `parent_task_id`
- `agent_id`
- `input`
- `output`
- `status`
- `depth`
- `attempt`
- `deadline_at`

Task state:

```text
pending -> ready -> running -> completed
pending -> canceled
ready -> running
running -> blocked -> running
running -> failed -> retry_scheduled -> ready
running -> canceled
running -> skipped
blocked -> canceled
```

Rules:
- `depth` enforces recursive delegation limits.
- Parent tasks cannot complete until required child tasks reach a terminal state.
- Concurrent child tasks are linked by a `task_group`.

### Task Group

A `task_group` models fan-out/fan-in across sub-agents or workflow branches.

Important fields:
- `task_group_id`
- `run_id`
- `parent_task_id`
- `mode`: `sequential`, `parallel`, `race`, `quorum`, `best_effort`
- `failure_policy`: `fail_fast`, `wait_all`, `quorum`, `best_effort`
- `quorum_count`
- `status`

Task group state:

```text
pending -> running -> completed
running -> failed
running -> canceled
running -> partially_completed
```

Rules:
- Parallel groups require policy approval before scheduling.
- Fan-in produces a deterministic aggregate event even when children finish out of order.

### Step

A `step` is one atomic action within a run: model request, model response, tool call, approval wait, sub-agent spawn, snapshot, or system transition.

Important fields:
- `step_id`
- `run_id`
- `task_id`
- `parent_step_id`
- `step_type`
- `status`
- `input`
- `output`
- `error`
- `idempotency_key`
- `started_at`, `ended_at`

Step state:

```text
pending -> running -> succeeded
pending -> canceled
running -> waiting -> running
running -> failed -> retry_scheduled -> pending
running -> timed_out
running -> canceled
```

Rules:
- Retriable steps need an idempotency key.
- Mutating tools require explicit retry policy.
- Step records are projections; event records remain canonical.

### Message and Event

`event` is the canonical ordered history. `message` is a user-facing or agent-facing projection for chat-like access.

Event categories:
- `human_message_created`
- `agent_message_created`
- `agent_trace_recorded`
- `system_event_recorded`
- `model_call_started`
- `model_call_completed`
- `tool_call_scheduled`
- `tool_call_started`
- `tool_call_completed`
- `approval_requested`
- `approval_resolved`
- `snapshot_created`
- `artifact_created`
- `task_spawned`
- `task_completed`
- `run_state_changed`
- `error_recorded`

Visibility classes:
- `public`: visible to API consumers and SSE public stream
- `participant`: visible to session participants
- `internal`: visible to operators and replay tools
- `hidden_reasoning`: stored separately and never mixed into public messages
- `audit`: immutable operational record

Rules:
- Events are append-only.
- Event order is `(run_id, sequence)` for run-local ordering and `(session_id, sequence)` for session-local ordering.
- Concurrent execution may finish out of order, but persisted event sequence is deterministic.

### Tool Definition and Tool Invocation

A `tool_definition` describes a native Go tool or MCP tool. A `tool_invocation` is one scheduled execution.

Important tool definition fields:
- `tool_id`
- `name`
- `version`
- `source`: `native`, `mcp`
- `input_schema`
- `output_schema`
- `idempotency_hint`
- `side_effect_level`
- `concurrency_class`
- `status`

Tool invocation state:

```text
scheduled -> running -> succeeded
scheduled -> canceled
running -> failed
running -> timed_out
running -> canceled
failed -> retry_scheduled -> scheduled
```

Concurrency classes:
- `serial`: default, scheduler runs exclusively per tool policy scope
- `parallel_read`: safe for concurrent read-only calls
- `parallel_isolated_write`: safe when idempotency and target isolation are proven
- `exclusive`: requires exclusive execution across configured scope
- `external_side_effect`: approval and idempotency policy required

### Approval

An `approval_request` blocks or gates execution.

Approval state:

```text
requested -> approved
requested -> rejected
requested -> expired
requested -> canceled
```

Rules:
- Approval decisions are audit events.
- Approvals should support both synchronous blocking and asynchronous callback resolution.

### Snapshot

A `snapshot` is an auditable compaction artifact for context reconstruction.

Important fields:
- `snapshot_id`
- `run_id`
- `covers_event_sequence_from`
- `covers_event_sequence_to`
- `summary`
- `model_call_ref`
- `created_by_step_id`

Rules:
- Raw events are never deleted because a snapshot exists.
- Replay may choose raw history or snapshot plus tail events.
- Snapshot generation itself is a model/tool step with stored inputs and outputs.

### Artifact

An `artifact` is a durable output beyond messages.

Artifact types:
- file
- JSON document
- report
- code patch
- test result
- model transcript

Rules:
- Artifacts are versioned.
- Each artifact links to producing run, task, and step.
- Large artifacts should store metadata in PostgreSQL and bytes in object storage or filesystem-backed blob storage.

### Memory

`memory_item` is long-term retrievable state, separate from transient session history.

Important fields:
- `memory_id`
- `tenant_id`
- `scope_type`: `tenant`, `project`, `session`, `workflow`, `agent`
- `scope_id`
- `content`
- `embedding`
- `provenance`
- `write_policy`
- `status`

Rules:
- Memory writes require policy. Not every message becomes memory.
- Retrieved memories must include provenance in model input metadata.

### Budget and Limit Ledger

Budget enforcement should be modeled as a ledger, not only counters on a run.

Tracked dimensions:
- prompt tokens
- completion tokens
- cached tokens
- total tokens
- estimated cost
- runtime duration
- model calls
- tool calls
- sub-agent count
- concurrency slots

Rules:
- Reservation happens before execution.
- Consumption is committed after execution.
- Cancellation releases unused reservations.

## Recommended Aggregates

Use these as Go aggregate boundaries:
- `SessionAggregate`: session metadata, participants, current message projection.
- `RunAggregate`: run state, limits, task graph, ordered events.
- `TaskAggregate`: task lifecycle, child task groups, current step.
- `ToolAggregate`: tool definition, policy, invocation lifecycle.
- `WorkflowAggregate`: template definition and versioned validation.
- `MemoryAggregate`: memory write/retrieval policies and provenance.

The event stream is the audit source of truth. Relational tables for runs, tasks, steps, and tool invocations are query projections optimized for APIs and operations.
