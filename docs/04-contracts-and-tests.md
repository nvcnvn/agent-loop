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
- `Last-Event-ID` resumes from the next event after the acknowledged sequence, with no gap
  between the replayed history and live events, and events arrive in sequence order.
- The stream closes after the run's terminal `run_state_changed` event, whether it is
  replayed from history or delivered live. A terminal event that later events follow ended an
  attempt of a run that was resumed afterwards, and does not close the stream.
- Public streams exclude `internal`, `hidden_reasoning`, and `audit` visibility.
- Heartbeats do not advance run sequence.
- With `?deltas=1` the stream also carries `agent_message_delta` events while a model writes a
  reply: `{"run_id", "task_id", "step_id", "agent", "content"}`, without an `id`. They are not
  stored, carry no reasoning, and may be dropped for a slow client or across a reconnect. Each
  model call has its own `step_id`; a reply that ends in tool calls or is rejected by the output
  schema is superseded by the next step, and the finished reply is `agent_message_created`, which
  follows its deltas.
- Event names are backward-compatible or versioned.

### 3. Workflow Definition Contract

A workflow is JSON (`runtime.Workflow`). A client sends it as `workflow` in `POST /v1/runs`, or
starts a named template from the server's config file (§4) with `"template": "<name>"`. The run
stores the definition it started with, so later config changes do not affect it. Durations are
nanoseconds, as in `limits`.

```json
{
  "name": "build-feature",
  "input_schema": {"type": "object", "required": ["feature"]},
  "stages": [
    {"name": "requirements", "agents": [{"name": "analyst", "prompt": "Specify {{input.feature}}."}]},
    {"name": "code", "agents": [{"name": "coder", "prompt": "Implement {{stage.requirements}}\nReview: {{feedback}}"}],
     "retry": {"max_attempts": 2, "backoff": 1000000000}, "timeout": 600000000000},
    {"name": "review", "agents": [{"name": "reviewer", "prompt": "Review {{stage.code}}. Start with APPROVED or REJECTED."}],
     "hand_back": {"when": "^REJECTED", "to": "code", "max_times": 3}},
    {"name": "test", "agents": [{"name": "tester", "prompt": "Test {{stage.code}}."}], "approve_output": true}
  ]
}
```

| Stage field | Meaning |
| --- | --- |
| `name` | Unique within the workflow; the run output maps it to the stage output. |
| `agents` | Agent steps: a registered agent's `name`, a `prompt` template, and `images`, the names of run input images shown with the prompt. |
| `kind` | `sequential` (default): steps run in order. `parallel`: steps run at once, up to `max_concurrent`, and the stage output is one `agent=answer` line per step. |
| `failure_policy` | For parallel stages: `fail_fast` (default), `wait_all` or `best_effort`. |
| `depends_on` | Stages that must finish first. When no stage sets it, each stage depends on the one before it. When any stage sets it, a stage without it starts at once, and stages whose dependencies have finished run concurrently. |
| `timeout` | Bounds the stage's agent work, not its approval gates. When it passes, the run ends `timed_out`. |
| `retry` | `max_attempts`, counting the first, and `backoff` before the first retry, doubled for each later one. A failed agent task is retried unless the run was canceled, hit its deadline or exhausted a budget. Each attempt is a task and counts as a sub-agent. |
| `hand_back` | When the stage output matches the regexp `when`, the stage `to` (this stage or one it depends on, directly or not) and every stage depending on it run again, after running stages finish. One hand-back more than `max_times` (default 3) fails the run. |
| `stop_when` | Regexp. When the stage output matches it, no further stage starts, and the run succeeds once running stages finish. |
| `output_schema` | JSON Schema every answer in the stage must satisfy. It is sent to the provider as the response schema. An answer that does not match goes back to the model with the validation error and counts toward the agent's `max_tool_loops`. |
| `approve_start`, `approve_output` | Ask for approval before the stage starts, or for its output. Any decision other than `approved` fails the run. Approving the last stage's output approves the final output. |

Prompt templates fill `{{input.<key>}}`, `{{stage.<stage>}}`, `{{stage.<stage>.<agent>}}`, and
`{{feedback}}`: the output that last handed work back to this stage, empty until one does.

A run's `images` (in `POST /v1/runs`: `{"<name>": {"mime_type": "image/png", "data": "<base64>"}}`)
are stored as the run's `input_image` artifacts and shown to the model with every step that names
them. Request bodies are capped at 1 MiB by default (`api.Server.MaxBodyBytes`).

`input_schema` is a JSON Schema that the run input, a JSON object of strings, must satisfy. Schemas
cannot reference other documents.

`StartRun` rejects an invalid workflow before creating a run (REST answers 400): a missing,
duplicate or unknown stage, agent, kind, failure policy or image; a dependency cycle; a hand-back to a stage
the handing stage does not depend on; an invalid regexp or schema; a negative timeout or retry; or
input that does not match `input_schema`.

The run records its workflow decisions as `system_event_recorded` events whose `event` is
`stage_handed_back`, `workflow_stopped`, `task_retry_scheduled` or `output_rejected`. Stage gates
use `approval_requested` and `approval_resolved` with `stage`, `gate` (`start` or `output`) and,
for output gates, `output`, where tool approvals have `tool`.

### 4. Agent Definition Contract

Agents are `runtime.Agent` values registered in Go, or come from the JSON file passed to
`cmd/agent-loop -config`, which also holds the named workflow templates. `runtime.ParseConfig`
rejects unknown fields, duplicate names, unknown child agents and invalid workflows.

```json
{
  "agents": [
    {"name": "lead", "role": "plans the research", "instructions": "Split the question, then delegate.",
     "children": ["researcher"], "max_tool_loops": 6, "timeout": 120000000000},
    {"name": "researcher", "role": "finds facts", "tools": ["echo"]}
  ],
  "workflows": [
    {"name": "research", "stages": [{"name": "answer", "agents": [{"name": "lead", "prompt": "{{input.question}}"}]}]}
  ]
}
```

- `model` defaults to the server's model, and every agent uses the server's provider.
- `tools` lists registry tools the agent may call; `max_tool_loops` (default 4) caps its model calls
  per task; `timeout` bounds each task attempt.
- An agent with `children` gets a built-in `delegate` tool taking `{"agent", "task"}`. The child runs
  as a task with the caller's task as its parent and one more level of depth, and its answer comes
  back as the tool result `{"agent", "answer"}`. Delegate calls in one model response run
  concurrently, up to `max_concurrent`. A delegated task counts as a tool call and a sub-agent; an
  exhausted budget fails the run. A call naming an agent outside `children` is refused back to the
  model, and agents at depth `max_depth` are not offered the tool.

Agent definitions are not versioned and cannot be changed through the API, and there are no model
profile, tool policy or memory policy references, per-agent default limits, or per-agent
visibility rules.

### 5. Tool Registration Contract

A tool is a `core.ToolDefinition` and a `tool.Executor`, registered by name in a `tool.Registry`.
The definition holds the name, version, description, input and output JSON Schema, idempotency
hint, side-effect level, concurrency class, whether calls need approval, and a per-call timeout.
Native Go tools are registered in Go (`tool.RegisterBuiltins` registers the demo tools). Input is
not yet validated against the schema.

MCP servers are listed under `mcp_servers` in the `-config` file (§4):

```json
"mcp_servers": [
  {"name": "files", "command": ["/usr/local/bin/files-mcp", "--root", "/srv/data"]},
  {"name": "tickets", "url": "https://mcp.example.com/mcp",
   "headers": {"Authorization": "Bearer ${TICKETS_TOKEN}"},
   "timeout": 20000000000, "max_retries": 2, "break_after": 5, "break_for": 30000000000}
]
```

- A server is a program spoken to over stdio (`command`) or a streamable HTTP endpoint (`url`).
  `${VAR}` in a header value is read from the environment at startup.
- At startup each server's tools are listed and registered as `<server>.<tool>`, the names agents
  list in `tools`. A server that cannot be reached, or a header naming an unset variable, stops
  startup. Tools a server adds later are not picked up until a restart.
- The schemas are copied, the version is the one the server reports, and the timeout is the
  server's `timeout` (default 30s). The tool's annotations, which are hints the server is trusted to
  give, set its class; a missing annotation takes the MCP default (not read-only, destructive,
  open-world):

  | Annotations | Side effect | Concurrency |
  | --- | --- | --- |
  | read-only, open-world | `read_external` | `parallel_read` |
  | read-only, closed-world | `none` | `parallel_read` |
  | destructive | `dangerous` | `serial` |
  | not destructive, open-world | `write_external` | `serial` |
  | not destructive, closed-world | `write_internal` | `serial` |

  Read-only and idempotent-hinted tools are `idempotent`; the rest are `unknown`.
- A call result marked as an error, or a JSON-RPC error from the server, goes back to the model as a
  tool error. Otherwise the output is the result's structured content or, without it,
  `{"content": "<text>"}`.
- Any other failure means the connection was lost. The session is dropped and the next attempt
  reconnects; an `idempotent` call is retried up to `max_retries` (default 2) more times within its
  timeout. After `break_after` (default 5) calls in a row that found no working connection, calls
  fail at once with "circuit open" for `break_for` (default 30s), and every further failure reopens
  the circuit until a call succeeds.

### 6. Model Provider Contract

Two adapters implement `model.Provider`:
- `model/ollama` calls `/api/chat` and reads what each model supports from `/api/show` once per
  model: `tools` sets tool calling, `vision` image input, `embedding` embeddings.
- `model/openai` calls an OpenAI-compatible `/chat/completions` (GLM, OpenAI, vLLM, Ollama's `/v1`).
  The protocol has no capability discovery, so `Provider.Caps` states what the endpoint supports.

Before its first model call, a task whose prompt has images or whose agent has tools checks the
capabilities, and fails with `model.ErrUnsupported` (and is not retried) when the model lacks either.

Messages carry images, and the model's reasoning as `thinking`, which the runtime stores only in the
model call's `hidden_payload`. The Ollama adapter sends it back with the conversation; the OpenAI
adapter reads it from `reasoning_content` or from a leading `<think>…</think>` and does not send it back.

`ChatRequest.Parameters` uses canonical names (`temperature`, `top_p`, `max_tokens`, `seed`, `stop`)
that each adapter maps to its own (`max_tokens` is Ollama's `num_predict`). Tool names are sent to
OpenAI-compatible servers with every character outside `[a-zA-Z0-9_-]` replaced by `_`, as OpenAI
requires, and mapped back in the response.

Errors: a non-2xx response is a `model.StatusError` carrying the status and `Retry-After`.
`model.WithRetry` retries transient failures (408, 429, 5xx, network errors and client timeouts) up to
4 tries with jittered exponential backoff from 500ms to 20s, honouring `Retry-After`, and stops when
the caller's context ends. `cmd/agent-loop` wraps every provider in it.

Usage reports prompt, completion and cached tokens. The OpenAI adapter prices them with
`Provider.Price` (cost units per million input, cached input and output tokens), which fills
`Usage.CostUnits` for the cost budget.

### 7. Replay Contract

A run stores what replaying it needs: its workflow, input, limits and input images; each model
call's full request (messages, tools, schema, parameters) in `model_call_started.hidden_payload.request`
and response in `model_call_completed.hidden_payload.response`; each tool call's input
(`tool_call_scheduled`) and output or error (`tool_call_completed`); and the event order.

Two kinds of replay use them:
- **Strict replay** (`runtime.Replay`) rebuilds the run's terminal state from its events and checks
  it against the stored run.
- **Re-execution** (`Orchestrator.ReExecute`, `POST /v1/runs/{rid}/reexecute`) runs the run again
  through the current runtime, as a child run (`parent_run_id`) in a new session that holds the
  history the original saw. Every model call is answered with the response recorded for an
  identical request, and every tool call with the result recorded for the same tool and input, in
  recorded order; approvals are not asked again, since their outcome is in the recorded result.
  The copy ends like the original unless the runtime now builds different requests or treats the
  same responses differently; a request that was never recorded fails the copy with
  `model.ErrNoFixture`, naming the request. A run recorded before requests were stored answers 422
  (`runtime.ErrNotRecorded`); one that has not finished answers 409.

Not yet stored: agent definition versions, budget reservations, and random seeds.

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
- auth: `api.Server.Auth` resolves every request outside the public routes (`/healthz`, `/readyz`,
  `/metrics`, the debug console's static files) to a tenant and principal, or answers 401.
  `api.APIKeys` maps a bearer key to one identity and ignores identity headers;
  `api.HeaderIdentity` trusts them and is for local development only
- authorization: tenant-scoped checks on every store method; any identity of a tenant may act on
  all of that tenant's data
- secrets: stored outside normal config tables by reference
- logs/traces: redaction before export

### 10. Storage and Artifact Contract

Define artifact storage rules:
- JSON and small reports may live in PostgreSQL
- large files go to object storage or filesystem blob storage
- every artifact has MIME type, checksum, size, version, producer step, and retention policy

### 11. Migration Contract

- Migrations are numbered files in `store/pg/migrations/` (`0001_<name>.sql`, no gaps).
  `pg.Store.Migrate`, run at startup, applies those newer than the latest version in
  `schema_migrations`, all in one transaction under an advisory lock, so a migration cannot use
  statements that refuse to run in a transaction (such as `CREATE INDEX CONCURRENTLY`).
- Migrations are forward-only; an applied file is never edited.
- A database already migrated by a newer binary is left alone, so every migration must keep
  working with the previous binary: destructive changes need a compatibility migration first, and
  enum changes are append-only.

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
- overloaded
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
4. Resume a run after process restart: completed agent tasks are reused, the interrupted task runs
   again, its usage carries over, and its open approvals are canceled.
5. Resume a failed or timed-out run: it goes back to `queued`, completed tasks are reused and the
   failed task runs again. Succeeded and canceled runs cannot be resumed.
6. Agents see the session's earlier human messages (always as user turns) and earlier runs' answers
   (as assistant turns) between their instructions and the stage prompt.

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
4. Stream model tokens into `agent_message_delta` SSE events (`TestSSEStreamDeltas`), and never
   retry a stream after its first delta (`TestRetryStreamOnlyBeforeFirstDelta`).

### Persistence and Replay

1. Store every model input/output, tool input/output, and policy decision for a run.
2. Replay from raw events and verify the same terminal state.
2a. Re-execute a run from its recorded model and tool outputs and reach the same result
    (`TestReExecuteReproducesRun`, and every scenario re-executes its runs); a changed prompt
    fails the copy (`TestReExecuteDetectsDivergence`).
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
3. Reject approval; the agent receives the rejection as the tool result and the run continues.
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

1. Prevent cross-tenant reads for sessions, runs, events, tools, artifacts, and memory, and
   cross-tenant run cancellation.
2. Redact secrets from logs and traces.
3. Verify audit records for user actions and approval decisions.
4. Enforce artifact retention policy.
5. Reject a request without a valid API key with 401; a valid key fixes the tenant and principal,
   whatever identity headers say.

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
