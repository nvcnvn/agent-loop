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

A session's participants each have a role in it: `owner` (also manages participants), `member`
(posts messages, starts and steers runs, resolves approvals) or `viewer` (reads). Its creator is its
first owner. A principal's tenant role (`admin`, `operator`, `member`, `viewer`, fixed by its API
key) decides whether participation matters: admins and operators act on every session of the
tenant, members as their session role allows, viewers only read.

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

The definition is a graph of stages (see `docs/04-contracts-and-tests.md` §3). Each stage names
its agent steps and may declare its dependencies, a retry policy, a timeout, a hand-back to an
earlier stage, a stop condition, an output schema and approval gates; the workflow declares an
input schema. Agents declare their allowed tools and child agents.

Today templates are named entries in the server's config file. They have no ID, version or status,
and a run stores the definition it started with instead of a reference to it.

### Run

A `run` is one execution attempt inside a session. It may contain multiple tasks, sub-agent runs, model calls, tool calls, approval waits, and snapshots.

Important fields:
- `run_id`
- `session_id`
- `tenant_id`
- `root_task_id`
- `workflow_id`
- `parent_run_id`
- `started_by`
- `status`
- `limits`
- `usage_totals`
- `failure_reason`
- `started_at`, `ended_at`

Run state:

```text
queued -> running -> succeeded
queued -> canceled
running -> awaiting_approval -> running   # approved, rejected or expired; a refused tool call goes back
                                          # to the agent, a refused stage gate then fails the run
running -> paused -> queued   # a detected loop paused it; resumed by a human
running -> canceling -> canceled
running -> failed
running -> timed_out
running -> succeeded
queued | running | awaiting_approval -> running   # resumed after an interruption
paused -> canceled
failed | timed_out -> queued   # resumed
failed -> superseded    # new run attempt replaces it
```

Rules:
- `run.status` is a projection of the event stream, not the source of truth.
- A run is resumable when no process is executing it and it is unfinished, paused, failed or timed
  out. Recovery after a restart resumes unfinished runs but leaves paused ones to a human.
  Resuming reuses the outputs of its completed tasks, marks interrupted tasks failed, runs
  interrupted and failed tasks again, and cancels its open approvals. Succeeded and canceled runs
  are final.
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
- A workflow stage's agent steps are tasks at depth 0. A task an agent creates with the `delegate`
  tool has the calling task as its parent and one more level of depth; the run's `max_depth` caps it.
- Parent tasks cannot complete until required child tasks reach a terminal state.
- A retry of a failed task is a new task with the next `attempt` number.
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

Which roles read which visibility is the deployment's `visibility` setting (see
`docs/04-contracts-and-tests.md` §1).

Rules:
- Events are append-only.
- Events of a run share the run ID as correlation ID; an agent task's events name its `task_spawned`
  event as parent, and a step's events name the step's previous event as cause.
- Every event names its actor: the human or service principal behind a request, the agent whose
  task caused it, or the system principal for the runtime's own actions.
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
scheduled -> failed     # refused: tool not available to the agent or not registered
running -> failed
running -> timed_out
running -> canceled
failed -> retry_scheduled -> scheduled
```

Concurrency classes:
- `serial`: default, scheduler runs exclusively per tool policy scope
- `parallel_read`: safe for concurrent read-only calls
- `parallel_isolated_write`: safe when idempotency and target isolation are proven
- `exclusive`: runs one call at a time across all runs in the process
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

A `snapshot` summarizes a session's earlier messages so agents see the summary plus the recent
messages instead of the whole history. It is a session message with role `snapshot` and `internal`
visibility, written by the run that made it, whose content is:
- `summary`: the text agents see first, as a user turn "Summary of the earlier conversation: …"
- `through_id`: the last summarized message; the messages after it, in the session's message order
  (time written, then ID), follow the summary
- `through`: when that message was written, which snapshots without `through_id` are cut at
- `messages`: how many messages the summary covers, across every earlier snapshot

Rules:
- A run compacts when it starts, if the messages after the latest snapshot exceed
  `Compaction.MaxMessages` or `Compaction.MaxTokens` (characters / 4). The `Compaction.Agent`
  folds the previous summary and all but the last `KeepRecent` messages into a new summary.
- Raw messages and events are never deleted because a snapshot exists.
- Generating a snapshot is a model call of the run, recorded like any other (request and response
  in `model_call_*` events, counted against the run's budget), followed by `snapshot_created` with
  the summary in its hidden payload. Re-executing the run replays it.
- Clients never see snapshot messages; agents see the latest one.

### Artifact

An `artifact` is a durable output beyond messages.

Artifact kinds: `input_image` (a run's input images), and what tools produce: `file`, `json`,
`report`, `patch`, `test_result`, or any other kind a tool names.

Rules:
- A tool produces artifacts by returning them with its result; agents save documents with the
  built-in `artifact.save` tool. Each links to its session, run, task and step, and names the tool
  that produced it (`producer`); the task gives the agent.
- An artifact has a name, kind, MIME type, size and metadata. Its bytes are stored inline in
  PostgreSQL, at most `Orchestrator.MaxArtifactBytes` (10 MiB by default) each.
- Saving a name again in the same session makes its next version: each artifact has a `version`,
  one more than the latest one of its name in the session, and every version stays readable.

### Memory

A `memory` is long-term retrievable state, separate from session messages: a statement an agent
saved, its embedding and the embedding model, and where it came from.

Fields:
- `memory_id`, `tenant_id`, `content`, `embedding`, `embed_model`, `created_at`
- `scope`: who can retrieve it. `tenant` (every run of the tenant), `workflow` (runs of the same
  workflow, by name) or `session` (runs in the same session, the default)
- `source`: provenance, the run, session, task and step that saved it

Rules:
- Nothing becomes memory on its own. An agent writes one only by calling `memory.save`, which it
  has only when granted, and only at the deployment's write scope (`MEMORY_WRITE_SCOPE`).
- `memory.search` returns the tenant's memories visible from the run's session and workflow,
  nearest first by cosine similarity among those made with the same embedding model, each with its
  scope, source and time.

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
