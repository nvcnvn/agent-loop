# Contracts and Test Scenarios

The requirements need more than entities, SQL, and interfaces before development starts. The following contracts should be treated as first-class inputs to implementation.

## Contracts to Define Before Development

### 1. Public REST API Contract

Define an OpenAPI document for:
- create session
- post human message
- start run
- resume run
- cancel run
- list runs
- list run events
- list artifacts
- approve or reject gated actions
- subscribe to run events through SSE

Key decisions:
- idempotency headers for `POST` endpoints
- tenant and principal identity propagation
- error response shape
- pagination and event cursor shape
- which fields are stable public API vs internal runtime details

### 2. SSE Event Contract

Define a versioned event envelope:

```json
{
  "id": "run-sequence-or-event-id",
  "event": "tool_call_completed",
  "data": {
    "schema_version": "2026-05-09",
    "tenant_id": "uuid",
    "session_id": "uuid",
    "run_id": "uuid",
    "sequence": 42,
    "visibility": "public",
    "occurred_at": "2026-05-09T00:00:00Z",
    "payload": {}
  }
}
```

Required behavior:
- `Last-Event-ID` resumes from the next event after the acknowledged sequence.
- Public streams exclude `internal`, `hidden_reasoning`, and `audit` visibility.
- Heartbeats do not advance run sequence.
- Event names are backward-compatible or versioned.

### 3. Workflow Definition Contract

Define JSON Schema for workflow templates. Minimum fields:
- workflow name and version
- input and output schema
- nodes with agent role, instructions, allowed tools, and stop conditions
- edges with condition expressions
- retry policy
- timeout and deadline
- approval gates
- explicit concurrency groups
- fan-in failure policy

Concurrency should be explicit in templates for v1. Dynamic agent delegation can request concurrency, but the scheduler should reject it unless policy allows it.

### 4. Agent Definition Contract

Define a versioned schema for agent definitions:
- name, role, instruction
- model profile reference
- tool policy reference
- memory policy reference
- allowed child agents
- default limits
- visibility rules for traces and messages

Agent definitions should be immutable after activation. Updates create a new version.

### 5. Tool Registration Contract

Define a Go and JSON contract for native and MCP tools:
- name and semantic version
- description
- input JSON Schema
- output JSON Schema
- idempotency hint
- side-effect level
- concurrency class
- timeout defaults
- retry eligibility
- approval requirements

Native Go tools should have typed input/output wrappers, but schemas should still be materialized for model providers and API inspection.

### 6. Model Provider Contract

Define provider capability mapping:
- chat completion
- streaming
- tool calling
- parallel tool calling
- embeddings
- structured output
- token accounting

Also define normalized errors:
- transient provider failure
- rate limited
- context length exceeded
- unsupported capability
- malformed tool call
- safety refusal
- provider authentication failure

### 7. Replay Contract

Define what deterministic replay means for v1:
- strict replay: use stored model/tool outputs and verify state transitions
- provider replay: re-call model provider with stored inputs for comparison
- tool replay: re-run idempotent tools only

Replay should store:
- model inputs and outputs
- tool inputs and outputs
- workflow and agent definition versions
- policy decisions
- budget reservations and commits
- random seeds when used
- event ordering

### 8. Policy and Budget Contract

Define a policy matrix for:
- tool allowlist and denylist
- read-only vs mutating tools
- approval-required tools
- recursion depth
- sub-agent count
- concurrency limits
- tool-call limits
- model budget
- wall-clock deadline

Budget enforcement should reserve before execution and commit or release afterward.

### 9. Security and Tenancy Contract

Decide v1 deployment mode explicitly:
- data model: multi-tenant capable
- initial deployment: single tenant by default
- auth: pluggable principal resolver
- authorization: tenant-scoped checks on every store method
- secrets: stored outside normal config tables by reference
- logs/traces: redaction before export

### 10. Storage and Artifact Contract

Define artifact storage rules:
- JSON and small reports may live in PostgreSQL
- large files go to object storage or filesystem blob storage
- every artifact has MIME type, checksum, size, version, producer step, and retention policy

### 11. Migration Contract

Define database migration rules before writing SQL:
- migrations are forward-only for production
- every migration has an idempotent test fixture
- destructive changes require a compatibility migration first
- enum changes are append-only in normal releases

### 12. Error Taxonomy

Define stable error categories:
- validation_error
- unauthorized
- forbidden
- not_found
- conflict
- budget_exceeded
- approval_required
- policy_denied
- provider_unavailable
- tool_failed
- replay_mismatch
- internal_error

Each error should include:
- code
- message
- retryable flag
- correlation ID
- optional diagnostic details for internal visibility

## Acceptance Test Scenarios

### Session and Run Lifecycle

1. Create a session, post a human message, start a run, receive ordered SSE events, and finish successfully.
2. Start a run with an idempotency key twice and verify only one run is created.
3. Cancel a running run and verify in-flight model/tool calls receive cancellation.
4. Resume a run after process restart and continue from the latest durable event.

### Sequential Multi-Agent Workflow

1. Execute `requirements -> code -> review -> test` as sequential tasks.
2. Force review failure and verify the workflow hands back to the code task.
3. Enforce max retry count and fail with a diagnostic reason.

### Concurrent Sub-Agent Workflow

1. Execute `planner -> research agents in parallel -> synthesizer`.
2. Verify child tasks run concurrently but persisted events have deterministic sequence.
3. Test `fail_fast`, `wait_all`, `quorum`, and `best_effort` fan-in policies.
4. Exhaust per-workflow concurrency and verify excess tasks wait rather than start.

### Tool Calling

1. Register a native Go tool and validate input schema before execution.
2. Reject a tool call denied by policy.
3. Require approval for an external side-effecting tool.
4. Run parallel read-only tool calls and fan in results.
5. Serialize exclusive tools even when scheduled concurrently.
6. Cancel a parallel tool group and verify all in-flight calls observe context cancellation.

### MCP Integration

1. Discover tools from a mock MCP server.
2. Translate MCP schemas into internal tool definitions.
3. Invoke an MCP tool with timeout and retry policy.
4. Trip a circuit breaker after repeated MCP failures.

### Model Abstraction

1. Run a workflow through a mock provider with deterministic responses.
2. Verify unsupported capabilities fail before model invocation.
3. Verify token accounting updates the budget ledger.
4. Stream model tokens and convert them into public SSE events.

### Persistence and Replay

1. Store every model input/output, tool input/output, and policy decision for a run.
2. Replay from raw events and verify the same terminal state.
3. Replay from snapshot plus tail events and verify the same terminal state.
4. Detect replay mismatch when a stored tool result differs from a recomputed result.

### Snapshot and Compaction

1. Trigger snapshot by message count.
2. Trigger snapshot by token count.
3. Verify raw history remains available after snapshot.
4. Verify snapshot generation creates auditable events and model call records.

### Budget and Safety

1. Stop a run when token budget is exhausted.
2. Stop a run when sub-agent budget is exhausted.
3. Pause or fail a run after repeated identical tool calls.
4. Pause or fail a run after repeated near-identical messages.
5. Detect long idle wait and emit diagnostic event.

### Human Approval

1. Block a dangerous tool call until approval.
2. Approve asynchronously and resume the run.
3. Reject approval and transition the run to a terminal rejected/failed state.
4. Expire approval and verify timeout handling.

### Memory and Retrieval

1. Write memory only when memory policy allows it.
2. Retrieve memories scoped by tenant, project, session, and workflow.
3. Include memory provenance in model input metadata.
4. Verify a denied memory write creates no permanent memory item.

### Observability and Operations

1. Emit traces across model calls, tool calls, and sub-agent boundaries.
2. Emit metrics for latency, token use, cost, queue depth, failures, retries, and tool usage.
3. Gracefully shut down workers and recover queued/running work after restart.
4. Apply backpressure when queue depth exceeds configured limits.

### Security

1. Prevent cross-tenant reads for sessions, runs, events, tools, artifacts, and memory.
2. Redact secrets from logs and traces.
3. Verify audit records for user actions and approval decisions.
4. Enforce artifact retention policy.

## Recommended First Development Slice

Build the smallest useful vertical slice:

1. PostgreSQL migrations for tenants, principals, sessions, runs, tasks, steps, events, messages, tools, and approvals.
2. Mock model provider with deterministic scripted responses.
3. Native Go tool registry with one read-only demo tool.
4. REST endpoints for session, message, start run, list events, and SSE subscribe.
5. Runtime that can execute one root agent, spawn sequential child tasks, and persist events.
6. Budget enforcement for max steps, max tool calls, max runtime, and max depth.
7. Acceptance tests for successful run, policy-denied tool, cancellation, and replay from events.

Do concurrent sub-agents and MCP after the event store, scheduler, and policy decisions are stable. Those features depend heavily on deterministic event ordering and cancellation semantics.
