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
- list and download artifacts (§10)
- list pending approvals
- approve or reject gated actions
- subscribe to run events through SSE

Approvals: `GET /v1/approvals` lists the caller's tenant's approvals that are still `requested`
and not expired, oldest first, as `{"approvals": [{"approval_id", "tenant_id", "run_id",
"step_id", "reason", "status", "request", "resolution", "created_at", "resolved_at",
"resolved_by", "expires_at"}], "next_after"}`. `?run_id=` narrows the list to one run (404 if the tenant has no
such run), and `?status=` picks `approved`, `rejected`, `expired`, `canceled` or `all` (where an
approval that expired before anyone closed it still shows as `requested`). Pages hold `?limit=`
approvals (1 to 100, default 100); a full page carries `next_after`, which `?after=` takes to get
the next. `request` holds the `tool` and its `input`, the stage `gate`, `stage` and, for output
gates, `output`, or for an agent's question `"gate": "input"` and its `question`: whoever may approve sees what they approve, so anyone who can read the session can read tool
inputs this way, though `tool_call_scheduled` is internal. `POST /v1/approvals/{aid}` with
`{"decision": "approved" | "rejected", "payload": {...}}` resolves one as the caller's principal,
which `resolved_by` and the `actor_id` of the `approval_resolved` event record (when the runtime
closes an approval that expired or whose run ended, `resolved_by` is omitted and `actor_id` is the
system principal); an approval already resolved or
expired answers 409.

Roles and membership: an API key fixes its principal's tenant role. `admin` and `operator` act on
every session of the tenant; `member` creates sessions and acts
in the sessions it participates in as its session role allows; `viewer` only reads the sessions it
participates in. A session's creator is its `owner`. Session roles: `owner` also manages
participants, `member` posts messages, starts, cancels, resumes and re-executes runs and resolves
their approvals, `viewer` reads the session, its messages, runs, events and artifacts. A caller who
may not read a session gets 404 for it and everything in it, as if it did not exist; one who may
read but not do more gets 403. Access is checked when a request arrives, so an SSE stream opened
before its caller was removed from the session runs on until the run ends. A run's idempotency key
belongs to its session: reusing it in another session answers 409. `GET /v1/sessions` and `GET /v1/approvals` list only what the caller
participates in, unless it is an admin or operator. The `/v1/debug/*` endpoints need admin or
operator.

Messages: `POST /v1/sessions/{sid}/messages` with `{"role", "content"}` stores a human message by
the caller (`human_message_created`) and answers 201 `{"event"}`. If a run of the session is waiting
on an agent's question (`ask_human`, §4), the oldest such question (by creation time) takes the
message as its answer, and the response adds `answered_run_id` and `answered_approval_id`.

With `"run": {"workflow" | "template", "input", "limits", "idempotency_key"}` (or an
`Idempotency-Key` header) the message instead starts a run in the session, as its input `message`
(and, like every earlier message, in the history its agents see), and answers no question. The run
is checked first: one that is invalid, or whose input already has `message`, answers 400 like
`POST /v1/runs` and posts nothing. A key an earlier post already used answers 201 `{"run_id",
"replayed": true}` and posts nothing again. Otherwise the response adds `run_id`; if the run could
still not start once the message was stored (the queue is full), it answers 201 with `run_error`
in the error shape (`code`, `message`, `retryable`).

Sessions: `POST /v1/sessions` takes `{"title", "project"}`; `project` is an optional label of at most
200 bytes, trimmed, that sessions share to share project memory, and `GET /v1/sessions?project=`
lists one project's. Any session creator may use any label, so a project is not an access
boundary.

Memory: when the server has an embedding model, `POST /v1/sessions/{sid}/memories` with
`{"name", "content", "scope"}` ingests a document, and `POST /v1/artifacts/{aid}/memories` with
`{"scope"}` a text or JSON artifact, into long-term memory, answering 201 `{"memories": [ids],
"scope"}`. Both need write access to the session. The text, at most 1 MiB (413 otherwise), is cut
into parts of at most 2000 bytes at paragraph breaks, or at whitespace inside a longer paragraph,
each embedded and saved with its `document` name, `part` number, the artifact's ID, run, task and
step, and the principal who ingested it (`by`) as provenance. Every part is saved, or none. `scope` is `session` (default), `project` (the session needs a project; 400 otherwise)
or `tenant` (admins and operators only, 403 otherwise). A binary artifact answers 415, and a server
without an embedding model 501.

Participants: `GET /v1/sessions/{sid}/participants` lists `{"participants": [{"tenant_id",
"session_id", "principal_id", "role", "joined_at"}]}`; `PUT
/v1/sessions/{sid}/participants/{principal}` with `{"role": "owner" | "member" | "viewer"}` adds a
participant or changes its role and answers the participant; `DELETE` removes one (204).

Visibility: each role reads the event and message visibilities the deployment's `visibility` map
gives it, in `GET /v1/runs/{rid}/events`, the SSE stream and `GET /v1/sessions/{sid}/messages`.
By default members and viewers read `public` and `participant`, operators also `internal` and
`audit`, and admins also `hidden_reasoning`. Events carry their `hidden_payload` only for roles that
read `hidden_reasoning`; the SSE envelope never does.

Cancellation: `POST /v1/runs/{rid}/cancel`, with an optional `{"reason": "..."}` of at most 1024
bytes, records an `audit` `system_event_recorded` event (`"event": "run_cancel_requested"`,
`reason`) whose `actor_id` is the caller's principal, then stops the run; a finished run answers
409. Audit events are served to the roles that read `audit` visibility.

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
    "payload": {},
    "event_id": "uuid",
    "correlation_id": "uuid",
    "actor_id": "uuid",
    "parent_event_id": "uuid",
    "causation_id": "uuid"
  }
}
```

Every event of a run has the run's ID as `correlation_id`. An event of an agent task has that
task's `task_spawned` event as `parent_event_id`, and a delegated task's `task_spawned` has its
parent task's. An event of a model or tool call step after the first has the step's previous event
as `causation_id` (`model_call_started` → `model_call_completed`; `tool_call_scheduled` →
`approval_requested` → `approval_resolved` → `tool_call_started` → `tool_call_completed`). The
referenced event may be internal and missing from a public stream. `parent_event_id` and
`causation_id` are omitted when empty.

`actor_id` names who caused the event: the principal behind a request (a human message, starting
a run, which records `run_state_changed` `queued` with reason `started`, resuming, cancelling or
resolving an approval), the agent whose task the event belongs to (`core.AgentPrincipal(name)`, the
UUIDv5 of the agent name in namespace `6f1d0c5e-8a3b-5f0e-9c47-2b7a4e1d9f60`), or
`core.SystemPrincipal` (`00000000-0000-0000-0000-000000000003`) for what the runtime does itself,
such as state changes and approvals that expire. A `task_spawned` event names who started the task:
the delegating agent, or the system for a stage's task. The run records who started it
(`StartedBy`; for a re-execution, who asked for it).

Required behavior:
- Events reach a stream as soon as they are stored, by any process sharing the Postgres database,
  which pushes them with `LISTEN/NOTIFY` and keeps doing so after a lost connection
  (`TestPg_PushesEventsAndApprovals`).
- `Last-Event-ID` resumes from the next event after the acknowledged sequence, with no gap
  between the replayed history and live events, and events arrive in sequence order.
- The stream closes after the run's terminal `run_state_changed` event, whether it is
  replayed from history or delivered live. A terminal event that later events follow ended an
  attempt of a run that was resumed afterwards, and does not close the stream.
- A stream carries only the visibilities the caller's role reads (§1), never hidden payloads.
- Heartbeats do not advance run sequence.
- With `?deltas=1` the stream also carries `agent_message_delta` events while a model writes a
  reply: `{"run_id", "task_id", "step_id", "agent", "content"}`, without an `id`. They are not
  stored, carry no reasoning, and may be dropped for a slow client or across a reconnect. Each
  model call has its own `step_id`; a reply that ends in tool calls or is rejected by the output
  schema is superseded by the next step, and the finished reply is `agent_message_created`, which
  follows its deltas.
- `error_recorded` reports a failure with a stable `code` and its `message`. With `scope: run`
  (visibility `participant`) it reports a run that ends `failed` or `timed_out`, just before its
  terminal `run_state_changed`. With `scope: task` (visibility `internal`, since the message can
  name internal hosts) it reports a failed agent task attempt, with `agent`, `stage`, `attempt` and
  `will_retry`; an attempt stopped because its run, its stage or a sibling agent failed, timed out
  or was canceled records none. Codes: `budget_exceeded`, `policy_denied`, `timed_out`, `canceled`,
  `unsupported_capability`, `loop_detected`, `paused` (a loop paused the run), `provider_unavailable`
  (a model call failed), and `failed` for the rest.
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
| `failure_policy` | For parallel stages: `fail_fast` (default) fails the stage with the first failure and cancels the other agents; `wait_all` waits for every agent and fails if any failed; `best_effort` fails only if all failed; `quorum` fails, canceling the others, once fewer than `quorum` agents can still succeed. A stage that does not fail has the answers of the agents that succeeded. |
| `quorum` | For `failure_policy: quorum`, which needs `kind: parallel`, and only then: how many agents must succeed, from 1 to the number of agents. |
| `depends_on` | Stages that must finish first. When no stage sets it, each stage depends on the one before it. When any stage sets it, a stage without it starts at once, and stages whose dependencies have finished run concurrently. |
| `timeout` | Bounds the stage's agent work, not its approval gates. When it passes, the run ends `timed_out`. |
| `retry` | `max_attempts`, counting the first, and `backoff` before the first retry, doubled for each later one. A failed agent task is retried unless the run was canceled, paused or hit its deadline, or the task exhausted a budget, was refused by policy (`policy_denied`), needed a capability its model lacks, or hit a loop. Each attempt is a task and counts as a sub-agent. |
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

`tool_policy` (§8) applies to the workflow's runs on top of the server's policy, so it can only
narrow it: a tool must pass both, and a call needs approval when either asks. A client sets a
policy for one run by sending the workflow with the run; a run that names a `template` cannot also
send a workflow or policy.

`StartRun` rejects an invalid workflow before creating a run (REST answers 400): a missing,
duplicate or unknown stage, agent, kind, failure policy or image; a `quorum` that is missing, out of
range or set without `failure_policy: quorum`; a dependency cycle; a hand-back to a stage
the handing stage does not depend on; an invalid regexp or schema; a negative timeout or retry; an
unknown side-effect level in `tool_policy`; or input that does not match `input_schema`.

The run records its workflow decisions as `system_event_recorded` events whose `event` is
`stage_handed_back`, `workflow_stopped`, `task_retry_scheduled` or `output_rejected`. Stage gates
use `approval_requested` and `approval_resolved` with `stage`, `gate` (`start` or `output`) and,
for output gates, `output`, where tool approvals have `tool`.

### 4. Agent Definition Contract

Agents are `runtime.Agent` values registered in Go, or come from the JSON file passed to
`cmd/agent-loop -config`, which also holds the named workflow templates, history compaction, the
tool policy, the loop response and cost approval threshold (§8), the artifact size cap
(`max_artifact_bytes`, §10), and in `visibility` which event visibilities each role reads.
`runtime.ParseConfig` rejects unknown fields, duplicate names, unknown child agents, invalid
workflows, a compaction whose agent is unknown or that has no trigger, an unknown side-effect level
in the tool policy, an unknown loop response, and negative thresholds or caps.

```json
{
  "agents": [
    {"name": "lead", "role": "plans the research", "instructions": "Split the question, then delegate.",
     "children": ["researcher"], "max_tool_loops": 6, "timeout": 120000000000},
    {"name": "researcher", "role": "finds facts", "tools": ["echo"]}
  ],
  "workflows": [
    {"name": "research", "stages": [{"name": "answer", "agents": [{"name": "lead", "prompt": "{{input.question}}"}]}]}
  ],
  "compaction": {"agent": "lead", "max_tokens": 6000, "keep_recent": 6, "task_max_tokens": 12000},
  "tool_policy": {"approve_side_effects": ["write_external", "dangerous"]},
  "loop_response": "pause",
  "approve_above": {"total_tokens": 200000},
  "visibility": {"member": ["public", "participant", "internal"]}
}
```

- `model` defaults to the server's model, and every agent uses the server's provider.
- `tools` lists registry tools the agent may call; `max_tool_loops` (default 4) caps its model calls
  per task; `timeout` bounds each task attempt.
- An agent with `"ask_human": true` gets a built-in `ask_human` tool taking `{"question"}`. The run
  waits for an approval whose `request` is `{"gate": "input", "question"}`; the next message posted
  to the session answers it, and the agent gets `{"answer": "<message>"}`. An approval resolved
  over REST answers with its payload's `answer`; one rejected, expired or canceled gives the agent
  `{"answered": false, "status"}`.
- An agent with `children` gets a built-in `delegate` tool taking `{"agent", "task"}`. The child runs
  as a task with the caller's task as its parent and one more level of depth, and its answer comes
  back as the tool result `{"agent", "answer"}`. Delegate calls in one model response run
  concurrently, up to `max_concurrent`. A delegated task counts as a tool call and a sub-agent; an
  exhausted budget fails the run. A call naming an agent outside `children` is refused back to the
  model, and agents at depth `max_depth` are not offered the tool.

Tenants define more agents over the API, in the same JSON without `name` (taken from the path):
- `GET /v1/agents` lists the agents the caller's runs may use, the file's (`"source": "config"`)
  and the tenant's (`"source": "api"`, with `version`), by name. Admins and operators see the
  tenant's agents' `instructions`, and the file's only when the server runs with `-debug`.
- `PUT /v1/agents/{name}` (admins) creates or replaces one of the tenant's agents and answers
  `{"name", "version"}`, the version counting from 1. A name is 1 to 64 letters, digits, `_`, `.`
  or `-`, and every tool and child must exist (400 otherwise); a name the file uses answers 409.
- `DELETE /v1/agents/{name}` (admins) removes one (204); the file's answer 409.

A tenant's agents run on the server's provider and, unless they name one, its model; a server
without a provider (`Orchestrator.Provider`) refuses them (400). A stored definition that no longer
decodes is skipped with a warning. Each tenant agent has its own `per_agent` concurrency bound, and
an agent is offered `delegate` only for the children that exist. A run resolves
its agents when it starts and again when it resumes, so a resumed or re-executed run uses the
definitions current then. The file's `tool_policy` (§8) applies to every agent. There are no model
profiles, per-agent tool or memory policies, per-agent default limits, or per-agent visibility
rules.

### 5. Tool Registration Contract

A tool is a `core.ToolDefinition` and a `tool.Executor`, registered by name in a `tool.Registry`.
The definition holds the name, version, description, input and output JSON Schema, idempotency
hint, side-effect level, concurrency class, whether calls need approval, and a per-call timeout.
Native Go tools are registered in Go (`tool.RegisterBuiltins` registers the demo tools). A tool
registered without a side-effect level is `dangerous`, and without a concurrency class `serial`.
Registering a tool whose input schema does not compile, or references another document, fails.
Every call's input is validated against the schema before it is approved or run; a call that does
not match goes back to the model as a tool error naming the mismatch, and still counts toward the
tool-call budget.

Each call carries its step ID as `tool.Call.IdempotencyKey`, the same on every retry. A call that
fails with `tool.ErrTransient`, or runs past its own timeout, is retried as
`Orchestrator.ToolRetry` allows (by default 3 attempts, backoff from 200ms doubling) when the tool is
`idempotent`, or `conditionally_idempotent` and so able to use the key; other tools are never
retried. Each retry records an internal `tool_call_retried` system event and counts toward the
run's tool calls and toward retry storms (§8); when the run has no tool calls left, the last error
goes back to the model instead. A tool reports what a call cost in `tool.Result.CostUnits`, which
`tool_call_completed` records as `cost_units` and which counts toward the run's cost budget: a cost
that passes `max_cost_units` fails the run once the call is recorded. A failed call that fails its
task records the error's code as `fatal`. MCP tools retry lost connections themselves (below).

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
  startup; a tool whose input schema does not compile is skipped with a warning. When the server
  sends `notifications/tools/list_changed`, and after every reconnect, the list is read again:
  new and changed tools are registered and tools the server dropped are unregistered, so a later
  call to one goes back to the model as an unknown tool. A task offers the model the tools
  registered when it started. A server tool whose name another tool already has (another server's,
  or a builtin) is skipped with a warning, and two servers may not share a name.
- The schemas are copied (a missing input schema becomes `{"type": "object"}`), the version is the
  one the server reports, and the timeout is the
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
  recorded order, with its recorded cost; approvals are not asked again, since their outcome is in
  the recorded result, and a call whose error failed its task (`fatal`) fails the copy's alike.
  The copy ends like the original unless the runtime now builds different requests or treats the
  same responses differently; a request that was never recorded fails the copy with
  `model.ErrNoFixture`, naming the request. A run recorded before requests were stored answers 422
  (`runtime.ErrNotRecorded`); one that has not finished answers 409.

Not yet stored: agent definition versions, budget reservations, and random seeds.

### 8. Policy and Budget Contract

The tool policy (`policy.ToolPolicy`: `Orchestrator.Policy` in Go, `tool_policy` in the `-config`
file) decides for each tool whether agents may call it and whether each call waits for approval:

```json
"tool_policy": {"allowlist": [], "denylist": ["shell.exec"], "approval_required": ["orders.refund"],
                "deny_side_effects": ["dangerous"], "approve_side_effects": ["write_external"],
                "budgets": {"search.web": {"max_calls": 20, "max_time": 60000000000, "max_cost_units": 500}}}
```

- A tool is denied when `denylist` names it, its side-effect level is in `deny_side_effects`, or
  `allowlist` is not empty and does not name it. A denied tool is not offered to the model (the
  task records a `tool_denied` system event), and a call to it goes back to the model as an error.
- An allowed tool needs approval when its definition says so, `approval_required` names it, or its
  side-effect level is in `approve_side_effects`. The levels are `none`, `read_external`,
  `write_internal`, `write_external` and `dangerous`; a tool registered without one, or an MCP tool
  without annotations, is `dangerous` (§5). Re-execution never asks for approval, and it uses the
  policy in force now, so a tool denied since the run fails the copy (§7).
- `budgets` limits, per run, how often a tool is called (retries and calls refused approval do not
  count), the time its calls' attempts take together, and the cost units they report. A call
  starts with at most the time left, which concurrent calls of the tool each get in full, so
  they can overrun it together. A call past a budget, or cut short by the time budget, goes back
  to the model as a tool error.
- A workflow's own `tool_policy` (§3) applies to its runs as well, under the same rules; each
  budget of either applies.

The rest of the policy matrix:
- recursion depth
- sub-agent count
- concurrency limits
- tool-call limits
- model budget
- wall-clock deadline

Budget enforcement should reserve before execution and commit or release afterward.

Loop detection (`policy.LoopDetector`) catches a run that is not making progress:
- `repeated_tool_call`: the same tool called with the same input 5 times in the run;
- `rejected_answers`: an agent task's last 3 answers that the output schema rejected are
  near-identical: as sets of lower-cased words, each pair has a Jaccard similarity of at least 0.9
  (`ReplyRepeats`, `ReplySimilarity`). An accepted answer, or a reply with tool calls, is never
  compared, so an agent that fixes its answer or pages through a tool is not stopped;
- `repeated_drafts`: the drafts a hand-back sent back to its target stage the last 3 times are
  near-identical in the same sense;
- `repeated_delegation`: one agent task delegated 3 near-identical tasks (agent and task text);
- `retry_storm`: 10 task retries in the run within a minute (`RetryStorm`, `RetryWindow`).

Each records an `audit` `system_event_recorded` event with `"event": "loop_detected"`, its `kind`
and details, and the `response`, which `Orchestrator.LoopResponse` (`loop_response` in the config
file) picks:
- `fail` (default): the run fails with `loop_detected`.
- `pause`: the run stops in `paused`, with the loop as its `failure_reason`, until
  `POST /v1/runs/{rid}/resume` resumes it (completed tasks are reused, as for any resume) or
  `POST /v1/runs/{rid}/cancel` cancels it. A restart does not resume paused runs.
- `approve`: the run asks for an approval whose `request` holds `"gate": "loop"`, the `kind` and
  details; if approved the detector forgets what it saw and the run carries on, otherwise it fails.
  A task that hit a loop is not retried, whatever its stage's retry policy. A pause stops a parallel
  stage whatever its `failure_policy`. A hand-back loop's approval is awaited before the run handles
  its other stages' results.

A run whose total tokens or cost units pass the positive fields of `Orchestrator.ApproveAbove`
(`approve_above` in the config file, e.g. `{"total_tokens": 200000, "cost_units": 5000}`) asks once
before its next model call for an approval whose `request` holds `"gate": "cost"`, the usage and the
threshold; concurrent tasks wait for the same answer. Without approval the model calls fail with
`policy_denied`. A resumed run asks again once it is past the threshold; re-execution never asks.

### 9. Security and Tenancy Contract

Decide v1 deployment mode explicitly:
- data model: multi-tenant capable
- initial deployment: single tenant by default
- auth: `api.Server.Auth` resolves every request outside the public routes (`/healthz`, `/readyz`,
  `/metrics`, the debug console's static files) to a tenant and principal, or answers 401.
  `api.APIKeys` maps a bearer key to one identity and ignores identity headers;
  `api.HeaderIdentity` trusts them and is for local development only
- authorization: tenant-scoped checks on every store method, then the role and session membership
  rules of §1 (`policy.Access`)
- secrets: stored outside normal config tables by reference
- logs/traces: redaction before export

### 10. Storage and Artifact Contract

- A tool returns artifacts in `tool.Result.Artifacts` (name, kind, MIME type, content, metadata).
  After a successful call the runtime stores each with the call's tenant, session, run, task and
  step and the tool's name as `producer`; an empty kind is `file` and an empty name the tool's
  name. Each gets an `artifact_created` event (visibility `participant`, with `artifact_id`,
  `name`, `kind`, `mime_type`, `size` and `producer`), and `tool_call_completed` lists their IDs
  in `artifacts`. If one cannot be stored, the call goes back to the model as an error, and those
  stored before it remain. Resume reads back only the run's own input images, never an
  artifact a tool produced, whatever its kind.
- `artifact.save` (in `tool.RegisterBuiltins`) takes `{"name", "content", "kind", "mime_type"}`
  and saves the content, `text/plain` by default.
- `GET /v1/runs/{rid}/artifacts` lists a run's artifacts without their content, as
  `{"artifacts": [{"artifact_id", "tenant_id", "session_id", "run_id", "task_id", "step_id",
  "producer", "kind", "name", "version", "mime_type", "size", "metadata", "created_at"}]}`, and `GET
  /v1/artifacts/{aid}` returns the content with its MIME type, as an attachment with
  `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`, since tools and models
  choose both.
- Saving a name again in a session stores its next `version` (1, 2, …), whatever its kind, so a run
  input image named like an earlier one is its next version too; `GET
  /v1/artifacts/{aid}/versions` lists every version of that artifact's name in its session, oldest
  first, in the same shape. Lists never carry content.
- Content is stored inline in PostgreSQL. A tool artifact or run input image larger than
  `Orchestrator.MaxArtifactBytes` (`max_artifact_bytes` in the config file; 10 MiB by default, zero
  for no cap) is refused: the tool call goes back to the model as an error, and a run with such an
  image does not start. Not yet: object storage for large files, and checksums.
- Re-execution answers tool calls from their recorded output, so its copy produces no artifacts.

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

### Agent Definition

1. Define agents over the API, use them in a tenant's runs only, and remove them
   (`TestTenantDefinedAgents`, `TestAgentsOverREST`, `storetest.Agents`).

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
7. Post a message that starts a run with it as input (`TestPostMessageStartsARun`).
8. An agent asks a question and the next human message continues its run; an unanswered question
   expires back to the agent (`TestAskHumanContinuesWithTheNextMessage`,
   `TestUnansweredQuestionGoesBackToTheAgent`).

### Sequential Multi-Agent Workflow

1. Execute `requirements -> code -> review -> test` as sequential tasks.
2. Force review failure and verify the workflow hands back to the code task.
3. Enforce max retry count and fail with a diagnostic reason.

### Concurrent Sub-Agent Workflow

1. Execute `planner -> research agents in parallel -> synthesizer`.
2. Verify child tasks run concurrently but persisted events have deterministic sequence.
3. Test `fail_fast`, `wait_all`, `quorum` (`TestParallelStageQuorum`), and `best_effort` fan-in
   policies.
4. Exhaust per-workflow concurrency and verify excess tasks wait rather than start.

### Tool Calling

1. Register a native Go tool and validate input schema before execution
   (`TestToolInputValidatedBeforeExecution`).
2. Reject a tool call denied by policy.
3. Require approval for an external side-effecting tool (`TestSideEffectPolicyRequiresApproval`).
4. Run parallel read-only tool calls and fan in results.
5. Serialize exclusive tools even when scheduled concurrently.
6. Cancel a parallel tool group and verify all in-flight calls observe context cancellation.

### MCP Integration

1. Discover tools from a mock MCP server.
2. Translate MCP schemas into internal tool definitions.
3. Invoke an MCP tool with timeout and retry policy.
4. Trip a circuit breaker after repeated MCP failures.
5. Pick up tools a server adds, changes or removes after startup, leaving other tools alone
   (`TestRegistryFollowsServerToolChanges`, `TestReconnectRefreshesTools`,
   `TestToolNamesOfOthersAreLeftAlone`).

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
5. Retry a failed tool call only when the tool is idempotent (`TestTransientToolFailuresAreRetriedWhenSafe`,
   `TestTimedOutCallOfAnIdempotentToolIsRetried`), and re-execute a run that a tool's cost ended
   (`TestReExecuteToolCostOverrun`).

### Snapshot and Compaction

1. Trigger snapshot by message count (`TestCompactionSummarizesOlderHistory`).
2. Trigger snapshot by token count (`TestCompactionByTokens`).
3. Verify raw history remains available after snapshot.
4. Verify snapshot generation creates auditable events and model call records.
5. Fold the previous summary into the next (`TestCompactionFoldsTheLastSummaryIntoTheNext`).
6. Keep facts from early turns through two summaries on a hosted model
   (`TestLiveLongSessionCompaction`, paid: `make test-paid`).
7. Summarize a task's older tool-loop rounds once they pass `task_max_tokens`, keeping the latest,
   and re-execute the run alike (`TestCompactionWithinATask`); fold one task summary into the next
   without summarizing a summary alone (`TestTaskCompactionDoesNotResummarizeItsSummary`).

### Budget and Safety

1. Stop a run when token budget is exhausted.
2. Stop a run when sub-agent budget is exhausted.
3. Pause or fail a run after repeated identical tool calls (`TestLoopDetectorTripsOnRepeats`,
   `TestLoopPausesRun`), or ask whether it may carry on (`TestLoopAsksForApproval`).
4. Pause or fail a run after repeated near-identical messages (`TestNearIdenticalRepliesFailTheRun`),
   hand-back drafts (`TestNearIdenticalHandBackDraftsFailTheRun`) or delegations
   (`TestRepeatedDelegationFailsTheRun`), and after a retry storm (`TestRetryStormFailsTheRun`).
5. Detect long idle wait and emit diagnostic event.
6. Limit a tool's calls, time and cost per run (`TestToolBudgets`, `TestEveryToolBudgetApplies`,
   `TestRejectedCallLeavesTheToolBudget`).

### Human Approval

1. Block a dangerous tool call until approval.
2. Approve asynchronously and resume the run.
3. Reject approval; the agent receives the rejection as the tool result and the run continues.
4. Expire approval and verify timeout handling.
5. Ask once before a run goes past its cost threshold (`TestCostThresholdAsksOnce`).

### Memory and Retrieval

1. Write memory only through `memory.save`, at the configured scope
   (`TestWriteScopeLimitsWhoFindsAMemory`).
2. Retrieve memories scoped by tenant, project, session, and workflow, nearest first, on every store
   (`storetest.Memories`, run by `TestMemStoreMemories` and `TestPg_Memories`).
3. Return memory provenance with each hit (`TestSaveThenSearchAcrossSessions`).
4. Verify a rejected memory write creates no memory (`TestSaveRejectsEmptyAndOversizedMemories`).
5. A hosted model saves facts in one session and recalls them in another
   (`TestLiveMemoryAcrossSessions`, paid).
6. Share project memory between the sessions of a project only (`TestProjectScope`).
7. Ingest documents and text artifacts in parts with their provenance (`TestIngestDocument`,
   `TestIngestIntoMemory`).

### Observability and Operations

1. Emit traces across model calls, tool calls, and sub-agent boundaries.
2. Emit metrics for latency, token use, cost, queue depth, failures, retries, and tool usage.
3. Gracefully shut down workers and recover queued/running work after restart.
4. Apply backpressure when queue depth exceeds configured limits.

### Security

1. Prevent cross-tenant reads for sessions, runs, events, tools, artifacts, and memory, and
   cross-tenant run cancellation.
2. Redact secrets from logs and traces.
3. Verify audit records for user actions and approval decisions (`TestCancelRunIsAudited`,
   `TestResolveApprovalEndpoint`, `storetest.Approvals`).
4. Enforce artifact retention policy.
5. Reject a request without a valid API key with 401; a valid key fixes the tenant and principal,
   whatever identity headers say.
6. Keep members and viewers to the sessions they participate in, and to what their session role
   allows; serve each role only the visibilities it may read (`TestRolesAndSessionMembership`,
   `TestListRunEventsFiltersInternalVisibility`, `storetest.Participants`).

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
