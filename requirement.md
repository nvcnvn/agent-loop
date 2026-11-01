# Agent Loop Requirements

`[x]` marks a requirement the code meets today, `[ ]` one it does not. A requirement that is only
partly met is split into the part that is done and the part that is not.

**Blocker:** marks an unmet requirement that must be done before a production deployment.

## 1. Goal

Build a Go-native multi-agent framework for long-running, tool-using, stateful AI workflows. The framework should support both chat-style sessions and structured agent workflows, with strong persistence, observability, and control over agent execution.

Primary goals:
- Run one or many agents inside the same session.
- Support sequential and concurrent sub-agent execution.
- Make agent runs resumable, inspectable, and replayable.
- Support tool calling through native Go functions and MCP.
- Expose a real-time API suitable for product integration.
- Start with local LLM support via Ollama, but keep the model layer provider-agnostic.

## 2. Technology Bias

- Language: Go 1.25+.
- Storage: PostgreSQL 18+.
- Vector search: pgvector.
- Runtime style: strongly typed, concurrency-safe, cancellation-aware.

## 3. Current Non-Goals

- Full visual workflow builder.
- Fine-tuning or model training.
- Cross-region distributed execution.
- Full ADK interoperability on day one.
- General-purpose sandboxing for arbitrary untrusted code execution.

## 4. Core Product Capabilities

### 4.1 Multi-Agent Orchestration

The framework must:
- [x] Run several agents in one run as workflow stages.
- [x] Spawn sub-agents from a parent agent at runtime (the `delegate` tool; each delegated task
  records its parent task and depth).
- [x] Support sequential and concurrent execution (sequential and parallel stages).
- [x] Support mixed graph-style execution (stages declare `depends_on`; independent stages run concurrently).
- [x] Allow workflow templates such as:
  - requirements -> code -> review -> test
  - planner -> multiple research agents in parallel -> synthesizer
- [x] Allow an agent to hand work back to a previous stage when validation fails (`hand_back`).
- [x] Enforce per-run limits on total spawned agents, sub-agent concurrency, tokens, runtime, model calls, and tool calls.
- [x] Enforce the recursion depth limit (`MaxDepth`).
- [x] Allow per-run limits that override the defaults.
- [x] Cap per-run limits at server-side maximums (`Orchestrator.MaxLimits`) so a client cannot raise them without bound.
  Cost is not capped by default; the OpenAI-compatible adapter reports it only when given prices.

The framework should define a clear execution model:
- `session`: long-lived collaboration container.
- `run`: one execution attempt inside a session.
- `agent`: a configured reasoning unit with model, tools, and policy.
- `task`: a unit of delegated work assigned to an agent.
- `step`: one atomic action within a run, such as model response, tool call, approval wait, or sub-agent spawn. Today a step is only a `step_id` on events; it has no stored record of its own.

### 4.2 Workflow Definition

The framework must support two ways to define behavior:
- [x] Dynamic delegation at runtime by an agent.
- [x] Predefined workflows, sent as JSON with each run or built in Go.
- [x] Named workflow templates stored in configuration and started by name.

Workflow definitions should support:
- [x] Agent role and instruction.
- [x] Agent definitions loaded from configuration (`cmd/agent-loop -config`).
- [x] Agent definitions managed through the API (`/v1/agents`, per tenant, by admins).
- [x] Allowed tools per agent.
- [x] Allowed child agents.
- [x] Retry policy (per stage: attempts and backoff).
- [x] Run timeout (`MaxRuntime`).
- [x] Timeouts per stage, per agent task attempt, and per tool call.
- [x] Stop after a maximum number of tool loops per agent.
- [x] Declared stop conditions (`stop_when`).
- [x] Human approval gates on tools.
- [x] Human approval gates on stages and their outputs; gating the last stage's output gates the final output.
- [x] Input and output schema (JSON Schema for the run input, a map of strings, and for each stage's answers).

## 5. State, Persistence, and Replay

### 5.1 Session Model

A session is a shared container that may include:
- [x] Human messages attributed to the principal that posted them.
- [x] Session participants and membership: the creator owns a session and adds owners, members
  and viewers; members and viewers reach only the sessions they participate in.
- [x] Multiple agents.
- [x] Multiple runs over time.
- [x] Full event history.

Each session must preserve:
- [x] Ordered message history.
- [x] Agent state transitions.
- [x] Tool invocations and results.
- [x] Run-to-task relationships.
- [x] Task-to-child-task relationships.
- [x] Artifacts produced during execution.

Chat-style sessions require:
- [x] Runs pass the session's prior messages to the model: the latest history summary, then human
  messages and earlier runs' agent answers written after it, the last `Orchestrator.MaxHistoryMessages`
  of them, between the instructions and the prompt.
- [x] Posting a human message can start a run (with the message as input) or continue one whose
  agent asked a question with `ask_human`.

### 5.2 Message Model

Support at least these message and event types:
- [x] Human message.
- [x] Agent visible message.
- [x] Agent internal reasoning or hidden trace.
- [x] System event.
- [x] Tool call.
- [x] Tool result.
- [x] Approval request and approval result.
- [x] Snapshot (an internal session message holding a history summary, and `snapshot_created`).
- [x] Error event (`error_recorded`, with a stable code, for failed task attempts and runs).

Requirements:
- [x] Hidden reasoning is stored separately from user-visible messages (`hidden_payload`).
- [x] Every event has an explicit visibility.
- [x] Visibility rules are configurable per role (`visibility` in the `-config` file); operators read
  internal and audit events, admins also hidden reasoning.
- [x] Every event includes timestamps.
- [x] Every event includes actor identity: the requesting principal, the agent, or the system principal.
- [x] Events in one run share a correlation ID (the run ID).
- [x] Events carry parent (the task's `task_spawned`) and causation (the step's previous event) references.

### 5.3 Snapshot and Compaction

Support snapshots that summarize earlier context to control token growth.

Requirements:
- [x] Trigger snapshots by message count or token count, checked at the start of each run
  (`Orchestrator.Compaction`, or `compaction` in the `-config` file).
- [x] Compact within a run: a task's tool-loop rounds before its latest are summarized once they pass
  `task_max_tokens`.
- [x] Preserve the raw history even when summarized.
- [x] Make snapshot generation auditable (a recorded model call and `snapshot_created`).
- [x] Allow replay from raw history (strict replay) or from snapshot plus tail (re-execution
  replays the summarizing call and sees the same summary and tail).

### 5.4 Resumability and Replay

The framework must:
- [x] Resume interrupted, failed and timed-out runs (`Orchestrator.Resume`, `POST /v1/runs/{rid}/resume`).
- [x] Retry failed steps safely: a tool call that failed transiently or timed out is retried only when
  the tool is idempotent, or conditionally so with the step's idempotency key; model calls are
  retried by `model.WithRetry`.
- [x] Replay a run for debugging by rebuilding its terminal state from stored events.
- [x] Store the workflow, input, limits, tool inputs and outputs, and model output for each run.
- [x] Store model requests (messages, tools, parameters).
- [x] Re-execute a run deterministically using recorded model and tool outputs (`Orchestrator.ReExecute`,
  `POST /v1/runs/{rid}/reexecute`).

## 6. Tool Calling

### 6.1 Native Go Tools

Support tools implemented as normal Go functions with a defined registration contract.

Requirements:
- [x] Registration contract.
- [x] Input and output JSON schema declared on each tool.
- [x] Input validated against the schema before approval and execution; a mismatch goes back to the
  model as a tool error.
- [x] Context propagation for run timeout and cancellation.
- [x] Per-tool timeout (`ToolDefinition.Timeout`).
- [x] Structured error handling: a tool error or panic, or a call to a tool the agent may not use, goes
  back to the model as an `{"error": ...}` result instead of failing the task. A refused call still
  counts toward the tool-call budget.
- [x] Tool metadata: name, description, version, idempotency hint, side-effect level.

### 6.2 MCP Tools

Support MCP-based tools and remote MCP servers.

Requirements:
- [x] Tool discovery (at startup, from the servers under `mcp_servers` in the `-config` file).
- [x] Picking up tools a server adds or removes after startup (its `tools/list_changed`
  notification, and every reconnect).
- [x] Schema translation into the framework's internal tool model.
- [x] Authentication (HTTP headers whose secrets come from the environment) and connection lifecycle
  handling (reconnect after a lost connection).
- [x] Timeout, retry, and circuit-breaker support.

### 6.3 Tool Policy Layer

The framework should support policy enforcement per agent or workflow:
- [x] Allowlist per agent; allowlist and denylist per orchestrator.
- [x] Tool policy set per workflow, and so per run for a workflow sent with the run (`tool_policy`).
- [x] Read-only vs mutating tool classes declared on each tool.
- [x] Policy acts on the side-effect class (`deny_side_effects`, `approve_side_effects`), set in Go or
  as `tool_policy` in the `-config` file.
- [x] Approval required before tools flagged or listed as needing it.
- [x] Max tool calls per run.
- [x] Max calls, cost or time budget per tool and run (`budgets` in the tool policy), and per-call
  timeouts (`timeouts`).

### 6.4 Concurrent Tool Execution

The framework should support concurrent tool calls when the workflow and tool policy allow it.

Requirements:
- [x] Allow an agent step to schedule multiple tool calls in parallel.
- [x] Run serial tools one at a time, in call order.
- [x] Keep exclusive and serial tools from overlapping parallel tools started earlier in the same batch.
- [x] Mark each tool with concurrency characteristics such as:
  - safe for parallel read operations
  - safe for parallel isolated writes
  - requires exclusive execution
  - non-idempotent or externally side-effecting
- [x] Support fan-out and fan-in patterns where multiple tool results are collected before the next reasoning step.
- [x] Partial failure handling for parallel sub-agent stages: fail-fast, wait-all, best-effort, quorum.
- [x] Partial failure handling for parallel tool groups: a failed call becomes an error result and the
  rest of the group still runs.
- [x] Concurrency limit per run for parallel sub-agents.
- [x] Concurrency limits globally (`MaxActiveRuns`), per tenant and per workflow (runs), per agent (model
  calls) and per tool (executions; `exclusive` tools run one at a time), set in `Orchestrator.Concurrency`.
  Parallel tool calls in one step are bounded by the run's `MaxConcurrent`.
- [x] Preserve deterministic event ordering in persisted run history even when execution is concurrent.
- [x] Propagate cancellation, timeout, and budget exhaustion to all in-flight tool calls.

Design rule:
- [x] Concurrent tool execution is opt-in by policy, not assumed safe by default (tools default to `serial`).

## 7. Model Abstraction Layer

Start with Ollama, but the API must be provider-agnostic.

Requirements:
- [x] Unified interface for chat completion and tool calling.
- [x] Streaming (both adapters; reply deltas reach SSE clients that ask with `?deltas=1`).
- [x] Embeddings (`model.Embedder`: Ollama `/api/embed`, OpenAI-compatible `/embeddings`).
- [x] Structured output used by the runtime (a stage's `output_schema` is sent as `ResponseSchema`).
- [x] Provider capability detection: the Ollama adapter asks the server what each model supports, and
  a task that needs images or tools its model lacks fails before the model is called.
- [x] Image input for vision models (named run input images shown with an agent step's prompt).
- [x] Model reasoning (`thinking`) kept as hidden reasoning and passed back in the conversation.
- [x] Model-specific parameter mapping (canonical `Parameters` names each adapter translates).
- [x] Retry and backoff (`model.WithRetry`).
- [x] Token accounting for prompt, completion, and total usage.
- [x] Token accounting for cached tokens and cost (cost from configured per-token prices).

Adapters: Ollama, and any OpenAI-compatible hosted API (tested on GLM 5.3).
- [x] A production-ready adapter: streaming, retries with backoff, cost, tested on GLM 5.3.

## 8. API and Protocols

### 8.1 External API

Expose HTTP APIs for:
- [x] Create session.
- [x] Post human message.
- [x] Start run.
- [x] Resume run.
- [x] List runs and events.
- [x] List and download artifacts.
- [x] Approve or reject gated actions.
- [x] List pending approvals (`GET /v1/approvals`).
- [x] Subscribe to run events.

### 8.2 Real-Time Delivery

Support HTTP SSE for streaming updates.

SSE requirements:
- [x] Ordered event delivery per run.
- [x] Event IDs for resume.
- [x] Heartbeats.
- [x] Reconnect-safe resume (`Last-Event-ID`).
- [x] Separation of public events and internal events.
- [x] Reply deltas while a model writes (`?deltas=1`, not stored).
- [x] Push-based delivery from PostgreSQL: appended events and approval decisions `NOTIFY` the
  process, whose one `LISTEN` connection wakes the waiting streams and approvals.

### 8.3 Compatibility Targets

- [x] Provide a clean REST API first.
- Keep room for future ADK-compatible inbound or outbound integration.
- [ ] Document which parts are stable public API versus internal runtime contracts (OpenAPI).

## 9. Safety and Control

### 9.1 Loop and Stall Detection

The framework must detect runaway behavior such as:
- [x] Repeated identical tool calls.
- [x] Repeated near-identical messages: answers an agent task keeps giving that the output schema
  rejects.
- [x] Repeated near-identical drafts across hand-backs.
- [x] Excessive delegation without progress: one task delegating near-identical tasks.
- [x] Retry storms: too many task retries in a run within a window.
- [x] Long idle waits: a run with no events for `Orchestrator.IdleWarnAfter` gets an `idle_wait`
  diagnostic event and a warning log naming the last event; unresolved approvals expire (§9.3).

Responses may include:
- [x] Warn (idle waits).
- [x] Pause run (`loop_response: pause`), until resumed or canceled.
- [x] Request human approval (`loop_response: approve`).
- [x] Fail run with diagnostic reason.

### 9.2 Budget Enforcement

Track and enforce:
- [x] Token budget.
- [x] Time budget.
- [x] Cost budget, when the provider reports cost.
- [x] Cost reported by providers (the OpenAI-compatible adapter, from `LLM_PRICE`).
- [x] Tool-call budget.
- [x] Sub-agent budget.
- [x] Budget usage persisted on the run (`usage_totals`) after every model call and tool reservation, and
  dropped from memory when the run ends.

### 9.3 Human-in-the-Loop

Support approval checkpoints for:
- [x] Dangerous tools and tools with external side effects, when flagged.
- [x] High-cost actions: a run past its token or cost threshold (`approve_above`) asks once before
  further model calls.
- [x] Final answer publication in selected workflows (`approve_output` on the last stage).
- [x] Approval expiry (`Orchestrator.ApprovalTimeout`): an expired approval can no longer be granted and
  the agent gets the expiry as the tool result. An approval still open when its run ends is canceled.
- [x] A rejection returned to the agent as a tool result instead of failing the run.

## 10. Observability and Operations

Requirements:
- [x] Structured logs (`log/slog`; `cmd/agent-loop` writes JSON to stdout): HTTP requests, run start and
  finish with usage, failed tool calls, and store writes that fail.
- [x] Traces across model calls, tool calls, and sub-agent boundaries (OpenTelemetry, exported over
  OTLP when configured; run logs carry the trace ID).
- [x] Metrics for latency, token use, cost, queue depth, failures, and tool usage at `/metrics`
  (Prometheus text; listed in docs/03), including task retries.
- [x] Audit trail of human messages and approvals in the event history.
- [x] Audit of run cancellations (who and why) and of who resolved each approval.
- [x] Run timeline view data model (events, tasks, and the debug console).

Operational requirements:
- [x] Graceful shutdown: on SIGINT/SIGTERM `cmd/agent-loop` stops accepting requests and runs, closes
  SSE streams, lets runs drain for `-shutdown-timeout`, then checkpoints the rest (left unfinished
  for `Recover`).
- [x] Job recovery after process restart: `cmd/agent-loop` calls `Orchestrator.Recover` at startup and
  then every lease period, which resumes every unfinished run no live process holds.
- [x] Liveness endpoint (`/healthz`).
- [x] Readiness endpoint that checks the database (`/readyz`).
- [x] Backpressure handling: at most `MaxActiveRuns` runs execute, up to `MaxQueuedRuns` more wait in
  `queued`, and StartRun beyond that fails with `core.ErrOverloaded` (REST 503 with `Retry-After`).
- [x] Configurable worker pool and queueing model (`concurrency` in the config file: runs executing,
  runs queued first-in-first-out, and per-tenant, workflow, agent and tool bounds).
- [x] Server timeouts (read header, read, idle) and request body size limits (`api.Server.HTTPServer`,
  `MaxBodyBytes`).
- [x] Versioned schema migrations (`store/pg/migrations`, applied by `pg.Store.Migrate`).
- [x] In-process state for finished runs is released; `Wait` answers finished runs from the store.
- [x] Run ownership by lease, so several API replicas can share one database: a process holds each run
  it executes by a renewed lease, other processes take a run over once its lease passes, and cancel
  it through the store. Reply deltas reach only the SSE streams of the process executing the run.
- [x] Deployment configuration for agents and workflow templates from a file (`-config`).
- [x] Deployment configuration for tools, policy, and limits from file or environment (`-config` or
  `AGENT_LOOP_CONFIG`, with `${VAR}` from the environment).

## 11. Security and Multi-Tenancy

Requirements:
- [x] Tenant-scoped reads and writes in both stores.
- [x] Tenant-scoped run cancellation.
- [x] Tenant-scoped tool registries: tools only one tenant's runs see (`tenant_mcp_servers`,
  `Orchestrator.TenantTools`) and tool policies per tenant (`tenant_tool_policies`).
- [x] Authentication and authorization: `/v1` requests authenticate with an API key that fixes their
  tenant and principal (`AGENT_LOOP_API_KEYS`), and every store call is scoped to that tenant. Trusting
  `X-Tenant-ID`/`X-Principal-ID` needs `-trust-identity-headers`.
- [x] Roles and permissions within a tenant: each API key has a role (`admin`, `operator`, `member`,
  `viewer`), combined with the principal's role in each session.
- [x] Secret management for tool credentials: tools ask for them by name (`tool.Call.Secrets`), from
  a mounted secret directory, per tenant, or the environment; no secret is stored.
- [x] Redaction support for logs and traces: known secrets and secret-shaped values, plus configured
  patterns, become `[REDACTED]`.
- [x] Data retention policy (`retention` in the config file): idle sessions are deleted with
  everything in them, and memories past an age.
- [x] Optional encryption at rest for sensitive fields (`AGENT_LOOP_ENCRYPTION_KEYS`: AES-256-GCM on the
  Postgres columns holding conversation and tool data, with key rotation).
- [x] Debug console and `/v1/debug/*` endpoints are off unless enabled (`-debug`); they expose agent instructions.
- [x] Single-tenant deployment is the initial scope (see §17).

## 12. Memory and Retrieval

Since PostgreSQL and pgvector are part of the stack, define memory behavior explicitly.

Requirements:
- [x] Store long-term memory separate from transient conversation state (`memories`, pgvector).
- [x] Support embeddings for retrieval over prior sessions (agents save and search with the
  `memory.save` and `memory.search` tools).
- [x] Ingest documents and text artifacts into memory (`POST /v1/sessions/{sid}/memories`,
  `POST /v1/artifacts/{aid}/memories`).
- [x] Scope retrieval by tenant, project, session, or workflow; a project is an optional label on
  sessions.
- [x] Allow memory write policies so not every message becomes permanent memory (only agents granted
  `memory.save` write, at the deployment's `MEMORY_WRITE_SCOPE`).
- [x] Track provenance for retrieved memories (run, session, task and step, or the ingested document,
  artifact, part and who ingested it, returned with each hit).

## 13. Artifacts and Outputs

Agents will often produce outputs beyond messages.

Support artifact storage for:
- [x] Files, structured JSON outputs, reports, code patches, and test results, stored with a kind, MIME type, and metadata.
- [x] Agents and tools producing artifacts during a run (`tool.Result.Artifacts`; agents use the
  built-in `artifact.save` tool).

Requirements:
- [x] Artifact versioning: saving a name again in a session stores its next version.
- [x] MIME type and metadata.
- [x] Link artifacts to run and task.
- [x] Link artifacts to step and producer (the tool).

## 14. Testing Strategy

Requirements:
- [x] Unit tests for workflow engine, persistence, budget enforcement, and policy checks.
- [x] Acceptance tests with mock LLM adapters.
- [x] Integration tests with Ollama in a controlled test environment.
- [x] Scenarios replayed from fixtures recorded on several small model families (Gemma 4, Qwen3-VL,
  Ministral 3), including image input.
- [x] Contract tests for tool registration and MCP integration (against the MCP SDK's server over
  stdio, streamable HTTP and in-memory transports).
- [x] Replay tests on runs produced in the test.
- [ ] Replay tests using recorded run fixtures.
- [x] Concurrency tests for fan-out and cancellation.
- [ ] Failure injection tests for network loss, tool timeout, and process restart.
- [x] Fuzz tests for every decoder of external input: model responses, streams, structured output and
  embeddings, model capabilities, MCP tool definitions and results, tool arguments (including
  `delegate`), REST bodies and queries, and API key lists.

## 15. End-State Goals

The framework should ultimately achieve these end goals:
- Serve as a reliable runtime for single-agent and multi-agent applications, not just a demo orchestration layer.
- Support chat, workflow, and long-running background execution in one consistent execution model.
- Allow agents to delegate work recursively while staying within explicit safety and budget constraints.
- Treat persistence, replay, auditability, and observability as core product capabilities.
- Support both dynamic agent behavior and predefined workflow graphs.
- Allow tools, models, memory, and policies to be swapped without changing the core runtime.
- Support both sequential and concurrent execution for sub-agents and tool calls.
- Be safe enough for production use with approval gates, policy enforcement, and failure containment.
- Be extensible enough to integrate remote tools, remote agents, and additional model providers later.
- Be deterministic enough for debugging, testing, and operational diagnosis.

The framework should be considered powerful when it can act as:
- an agent runtime
- a workflow engine
- a collaboration timeline
- a tool execution layer
- an auditable operations surface

## 16. Success Criteria

The product vision is met when the framework can:
- [x] Accept human requests through an API and stream real-time execution events.
- [x] Execute workflows that combine planning, delegation, tool use, review, and retry.
- [x] Spawn multiple agents concurrently and merge their outputs safely.
- [x] Schedule multiple tool calls concurrently when policy permits.
- [x] Persist enough state to inspect and audit any run.
- [ ] Persist enough state to resume and deterministically replay any run.
- [x] Apply budgets, limits, and approval gates without relying on prompt discipline alone.
- [x] Support long-term memory and retrieval with clear provenance.
- [x] Expose operational telemetry sufficient for debugging and production monitoring.
- [x] Remain provider-agnostic at the model layer.
- [x] Remain protocol-agnostic at the tool layer (native Go and MCP tools).
- [x] Pass deterministic or replay-based test scenarios for important workflows.

## 17. Design Decisions

- Tenancy: the initial deployment is single-tenant. Every record carries a `tenant_id`; an API key
  fixes the tenant of its requests (with `-trust-identity-headers`, requests without `X-Tenant-ID` use
  `core.DefaultTenant`).
- Hidden reasoning: persisted by default in `hidden_payload` and never served by the public REST or
  SSE endpoints.
- Workflow definitions: both. Tools are registered in Go; agents are registered in Go or loaded from
  the `-config` file; workflows are JSON sent with each run, Go structs, or named templates from the
  config file.
- Replay: a best-effort debugging feature that rebuilds state from stored events, not a hard
  determinism guarantee.
- Native tools run in-process; MCP tools run in their servers, reached over stdio or streamable HTTP.
- Approvals are asynchronous: the run waits in `awaiting_approval` until a client resolves the
  approval over REST.
- Session history is linear.
- Tools default to `serial`; `parallel_read` and `parallel_isolated_write` tools run concurrently.
  Within one model response, any other tool is a barrier: it starts after the parallel tools before it
  finish, and the parallel tools after it start once it finishes.
- Concurrent tool groups are inferred by the scheduler from each tool's concurrency class, not
  declared in the workflow.

- Resuming a run reuses the outputs of its completed agent tasks and reruns an interrupted task from
  its start, so steps need no stored records. The interrupted attempt stays in the run's usage, and
  the resumed run gets a fresh `MaxRuntime`.

- Artifacts: stored inline in PostgreSQL, and refused above `max_artifact_bytes` (10 MiB by default).

Still open:
- Object storage for artifacts too large to keep inline.

## 18. First Implementation Milestones

The first implementation is viable when the framework can:
- [x] Accept a human request through REST.
- [x] Start a run and stream events through SSE.
- [x] Spawn sub-agents sequentially and concurrently.
- [x] Persist all major run events in PostgreSQL.
- [x] Call native Go tools.
- [x] Call MCP tools.
- [x] Execute safe tool calls concurrently with cancellation.
- [x] Bound concurrent tool calls with limits.
- [x] Enforce limits and stop runaway runs.
- [x] Replay a failed run.
- [x] Resume a failed run.
- [x] Produce observable logs, metrics, and traceable diagnostics.
- [x] Pass acceptance and integration test suites.
