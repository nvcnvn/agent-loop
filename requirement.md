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
- [ ] Spawn sub-agents from a parent agent at runtime (no task has a parent task or depth today).
- [x] Support sequential and concurrent execution (sequential and parallel stages).
- [ ] Support mixed graph-style execution (stages with dependencies instead of a linear list).
- [x] Allow workflow templates such as:
  - requirements -> code -> review -> test
  - planner -> multiple research agents in parallel -> synthesizer
- [ ] Allow an agent to hand work back to a previous stage when validation fails.
- [x] Enforce per-run limits on total spawned agents, sub-agent concurrency, tokens, runtime, model calls, and tool calls.
- [ ] Enforce the recursion depth limit (`MaxDepth` is defined but nothing recurses yet).
- [x] Allow per-run limits that override the defaults.
- [x] Cap per-run limits at server-side maximums (`Orchestrator.MaxLimits`) so a client cannot raise them without bound.
  Cost is not capped by default because no provider reports it yet.

The framework should define a clear execution model:
- `session`: long-lived collaboration container.
- `run`: one execution attempt inside a session.
- `agent`: a configured reasoning unit with model, tools, and policy.
- `task`: a unit of delegated work assigned to an agent.
- `step`: one atomic action within a run, such as model response, tool call, approval wait, or sub-agent spawn. Today a step is only a `step_id` on events; it has no stored record of its own.

### 4.2 Workflow Definition

The framework must support two ways to define behavior:
- [ ] Dynamic delegation at runtime by an agent.
- [x] Predefined workflows, sent as JSON with each run or built in Go.
- [ ] Named workflow templates stored in configuration and started by name.

Workflow definitions should support:
- [x] Agent role and instruction (agents are registered in Go at startup).
- [ ] Agent definitions loaded from configuration or the API.
- [x] Allowed tools per agent.
- [ ] Allowed child agents.
- [ ] Retry policy.
- [x] Run timeout (`MaxRuntime`).
- [ ] Timeouts and deadlines per stage, agent, and tool.
- [x] Stop after a maximum number of tool loops per agent.
- [ ] Declared stop conditions.
- [x] Human approval gates on tools.
- [ ] Human approval gates on stages and final outputs.
- [ ] Input and output schema (input is a flat string map; output is free text).

## 5. State, Persistence, and Replay

### 5.1 Session Model

A session is a shared container that may include:
- [x] Human messages attributed to the principal that posted them.
- [ ] Session participants and membership.
- [x] Multiple agents.
- [x] Multiple runs over time.
- [x] Full event history.

Each session must preserve:
- [x] Ordered message history.
- [x] Agent state transitions.
- [x] Tool invocations and results.
- [x] Run-to-task relationships.
- [ ] Task-to-child-task relationships.
- [ ] Artifacts produced during execution.

Chat-style sessions require:
- [ ] **Blocker:** Runs pass the session's prior messages to the model; today an agent sees only its instructions and stage prompt.
- [ ] Posting a human message can start or continue a run.

### 5.2 Message Model

Support at least these message and event types:
- [x] Human message.
- [x] Agent visible message.
- [x] Agent internal reasoning or hidden trace.
- [x] System event.
- [x] Tool call.
- [x] Tool result.
- [x] Approval request and approval result.
- [ ] Snapshot.
- [ ] Error event (failures appear only as a reason on the run state change).

Requirements:
- [x] Hidden reasoning is stored separately from user-visible messages (`hidden_payload`).
- [x] Every event has an explicit visibility.
- [ ] Visibility rules are configurable, with an internal-scope reader for operators.
- [x] Every event includes timestamps.
- [ ] Every event includes actor identity (only human messages set it).
- [ ] Events in one run share a correlation ID (each event gets a fresh random one).
- [ ] Events carry parent and causation references.

### 5.3 Snapshot and Compaction

Support snapshots that summarize earlier context to control token growth.

Requirements:
- [ ] Trigger snapshots by message count, token count, or workflow boundary.
- [ ] Preserve the raw history even when summarized.
- [ ] Make snapshot generation auditable.
- [ ] Allow replay from raw history or from snapshot plus tail events.

### 5.4 Resumability and Replay

The framework must:
- [ ] **Blocker:** Resume interrupted runs.
- [ ] Retry failed steps safely.
- [x] Replay a run for debugging by rebuilding its terminal state from stored events.
- [x] Store the workflow, input, limits, tool inputs and outputs, and model output for each run.
- [ ] Store model requests (messages, tools, parameters).
- [ ] Re-execute a run deterministically using recorded model and tool outputs.

## 6. Tool Calling

### 6.1 Native Go Tools

Support tools implemented as normal Go functions with a defined registration contract.

Requirements:
- [x] Registration contract.
- [x] Input and output JSON schema declared on each tool.
- [ ] Input validated against the schema before execution.
- [x] Context propagation for run timeout and cancellation.
- [ ] Per-tool timeout.
- [x] Structured error handling: a tool error or panic, or a call to a tool the agent may not use, goes
  back to the model as an `{"error": ...}` result instead of failing the task. A refused call still
  counts toward the tool-call budget.
- [x] Tool metadata: name, description, version, idempotency hint, side-effect level.

### 6.2 MCP Tools

Support MCP-based tools and remote MCP servers.

Requirements:
- [ ] Tool discovery.
- [ ] Schema translation into the framework's internal tool model.
- [ ] Authentication and connection lifecycle handling.
- [ ] Timeout, retry, and circuit-breaker support.

### 6.3 Tool Policy Layer

The framework should support policy enforcement per agent or workflow:
- [x] Allowlist per agent; allowlist and denylist per orchestrator.
- [ ] Tool policy set per workflow or run.
- [x] Read-only vs mutating tool classes declared on each tool.
- [ ] Policy acts on the class (for example, approval for every `write_external` or `dangerous` tool).
- [x] Approval required before tools flagged or listed as needing it.
- [x] Max tool calls per run.
- [ ] Max cost or time budget per tool.

### 6.4 Concurrent Tool Execution

The framework should support concurrent tool calls when the workflow and tool policy allow it.

Requirements:
- [x] Allow an agent step to schedule multiple tool calls in parallel.
- [x] Run serial tools one at a time, in call order.
- [ ] **Blocker:** Keep exclusive and serial tools from overlapping parallel tools started earlier in the same batch.
- [x] Mark each tool with concurrency characteristics such as:
  - safe for parallel read operations
  - safe for parallel isolated writes
  - requires exclusive execution
  - non-idempotent or externally side-effecting
- [x] Support fan-out and fan-in patterns where multiple tool results are collected before the next reasoning step.
- [x] Partial failure handling for parallel sub-agent stages: fail-fast, wait-all, best-effort.
- [ ] Quorum failure handling for parallel sub-agent stages.
- [x] Partial failure handling for parallel tool groups: a failed call becomes an error result and the
  rest of the group still runs.
- [x] Concurrency limit per run for parallel sub-agents.
- [ ] **Blocker:** Concurrency limits globally, per tenant, per workflow, per agent, and per tool; parallel tool calls in one step are unbounded.
- [x] Preserve deterministic event ordering in persisted run history even when execution is concurrent.
- [x] Propagate cancellation, timeout, and budget exhaustion to all in-flight tool calls.

Design rule:
- [x] Concurrent tool execution is opt-in by policy, not assumed safe by default (tools default to `serial`).

## 7. Model Abstraction Layer

Start with Ollama, but the API must be provider-agnostic.

Requirements:
- [x] Unified interface for chat completion and tool calling.
- [ ] Streaming.
- [ ] Embeddings.
- [ ] Structured output used by the runtime (the Ollama adapter maps `ResponseSchema`, but nothing sets it).
- [ ] Provider capability detection (capabilities are static).
- [ ] Model-specific parameter mapping.
- [ ] Retry and backoff.
- [x] Token accounting for prompt, completion, and total usage.
- [ ] Token accounting for cached tokens and cost.

Future providers may include hosted APIs, but the first production implementation only needs one production-ready adapter.
- [ ] A production-ready adapter (Ollama today is non-streaming, with no retry).

## 8. API and Protocols

### 8.1 External API

Expose HTTP APIs for:
- [x] Create session.
- [x] Post human message.
- [x] Start run.
- [ ] Resume run.
- [x] List runs and events.
- [ ] List and download artifacts.
- [x] Approve or reject gated actions.
- [ ] List pending approvals (clients learn approval IDs only from the event stream).
- [x] Subscribe to run events.

### 8.2 Real-Time Delivery

Support HTTP SSE for streaming updates.

SSE requirements:
- [x] Ordered event delivery per run.
- [x] Event IDs for resume.
- [x] Heartbeats.
- [x] Reconnect-safe resume (`Last-Event-ID`).
- [x] Separation of public events and internal events.
- [ ] Push-based delivery from PostgreSQL (`LISTEN/NOTIFY` instead of polling).

### 8.3 Compatibility Targets

- [x] Provide a clean REST API first.
- Keep room for future ADK-compatible inbound or outbound integration.
- [ ] Document which parts are stable public API versus internal runtime contracts (OpenAPI).

## 9. Safety and Control

### 9.1 Loop and Stall Detection

The framework must detect runaway behavior such as:
- [x] Repeated identical tool calls.
- [ ] Repeated near-identical messages.
- [ ] Excessive delegation without progress.
- [ ] Retry storms.
- [ ] **Blocker:** Long idle waits, including approvals that are never resolved.

Responses may include:
- [ ] Warn.
- [ ] Pause run.
- [ ] Request human approval.
- [x] Fail run with diagnostic reason.

### 9.2 Budget Enforcement

Track and enforce:
- [x] Token budget.
- [x] Time budget.
- [x] Cost budget, when the provider reports cost.
- [ ] Cost reported by providers (none do today).
- [x] Tool-call budget.
- [x] Sub-agent budget.
- [ ] **Blocker:** Budget usage persisted so it survives a restart and is released when a run ends.

### 9.3 Human-in-the-Loop

Support approval checkpoints for:
- [x] Dangerous tools and tools with external side effects, when flagged.
- [ ] High-cost actions.
- [ ] Final answer publication in selected workflows.
- [ ] **Blocker:** Approval expiry.
- [ ] **Blocker:** A rejection returned to the agent as a result instead of failing the run.

## 10. Observability and Operations

Requirements:
- [x] Structured logs (`log/slog`; `cmd/agent-loop` writes JSON to stdout): HTTP requests, run start and
  finish with usage, failed tool calls, and store writes that fail.
- [ ] Traces across model calls, tool calls, and sub-agent boundaries.
- [ ] **Blocker:** Metrics for latency, token use, cost, queue depth, failures, retries, and tool usage.
- [x] Audit trail of human messages and approvals in the event history.
- [ ] Audit of run cancellations and of who resolved each approval.
- [x] Run timeline view data model (events, tasks, and the debug console).

Operational requirements:
- [ ] **Blocker:** Graceful shutdown: stop accepting work, drain or checkpoint in-flight runs.
- [ ] **Blocker:** Job recovery after process restart (runs left `running` stay that way forever).
- [x] Liveness endpoint (`/healthz`).
- [x] Readiness endpoint that checks the database (`/readyz`).
- [ ] **Blocker:** Backpressure handling (every run starts its own goroutine at once).
- [ ] Configurable worker pool and queueing model.
- [x] Server timeouts (read header, read, idle) and request body size limits (`api.Server.HTTPServer`,
  `MaxBodyBytes`).
- [x] Versioned schema migrations (`store/pg/migrations`, applied by `pg.Store.Migrate`).
- [ ] **Blocker:** In-process state for finished runs is released (the run and budget maps grow without bound).
- [ ] Run ownership by lease, so several API replicas can share one database.
- [ ] Deployment configuration for agents, tools, policy, and limits from file or environment.

## 11. Security and Multi-Tenancy

Requirements:
- [x] Tenant-scoped reads and writes in both stores.
- [x] Tenant-scoped run cancellation.
- [ ] Tenant-scoped tool registries.
- [ ] **Blocker:** Authentication and authorization (`X-Tenant-ID` and `X-Principal-ID` are trusted as sent).
- [ ] Secret management for tool credentials.
- [ ] Redaction support for logs and traces.
- [ ] Data retention policy.
- [ ] Optional encryption at rest for sensitive fields.
- [x] Debug console and `/v1/debug/*` endpoints are off unless enabled (`-debug`); they expose agent instructions.
- [x] Single-tenant deployment is the initial scope (see §17).

## 12. Memory and Retrieval

Since PostgreSQL and pgvector are part of the stack, define memory behavior explicitly.

Requirements:
- [ ] Store long-term memory separate from transient conversation state.
- [ ] Support embeddings for retrieval over prior sessions, documents, and artifacts.
- [ ] Scope retrieval by tenant, project, session, or workflow.
- [ ] Allow memory write policies so not every message becomes permanent memory.
- [ ] Track provenance for retrieved memories.

## 13. Artifacts and Outputs

Agents will often produce outputs beyond messages.

Support artifact storage for:
- [x] Files, structured JSON outputs, reports, code patches, and test results, stored with a kind, MIME type, and metadata.
- [ ] Agents and tools producing artifacts during a run (nothing writes to the artifact store yet).

Requirements:
- [ ] Artifact versioning.
- [x] MIME type and metadata.
- [x] Link artifacts to run and task.
- [ ] Link artifacts to step and producer.

## 14. Testing Strategy

Requirements:
- [x] Unit tests for workflow engine, persistence, budget enforcement, and policy checks.
- [x] Acceptance tests with mock LLM adapters.
- [x] Integration tests with Ollama in a controlled test environment.
- [ ] Contract tests for tool registration and MCP integration.
- [x] Replay tests on runs produced in the test.
- [ ] Replay tests using recorded run fixtures.
- [x] Concurrency tests for fan-out and cancellation.
- [ ] Failure injection tests for network loss, tool timeout, and process restart.
- [ ] Fuzz tests for every decoder of external input (model responses, tool arguments, REST bodies).

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
- [ ] Execute workflows that combine planning, delegation, tool use, review, and retry.
- [x] Spawn multiple agents concurrently and merge their outputs safely.
- [x] Schedule multiple tool calls concurrently when policy permits.
- [x] Persist enough state to inspect and audit any run.
- [ ] Persist enough state to resume and deterministically replay any run.
- [x] Apply budgets, limits, and approval gates without relying on prompt discipline alone.
- [ ] Support long-term memory and retrieval with clear provenance.
- [ ] Expose operational telemetry sufficient for debugging and production monitoring.
- [x] Remain provider-agnostic at the model layer.
- [ ] Remain protocol-agnostic at the tool layer (native tools only; no MCP).
- [x] Pass deterministic or replay-based test scenarios for important workflows.

## 17. Design Decisions

- Tenancy: the initial deployment is single-tenant. Every record carries a `tenant_id`, and requests
  without `X-Tenant-ID` use `core.DefaultTenant`.
- Hidden reasoning: persisted by default in `hidden_payload` and never served by the public REST or
  SSE endpoints.
- Workflow definitions: both. Agents and tools are registered in Go; workflows are JSON sent with
  each run or Go structs.
- Replay: a best-effort debugging feature that rebuilds state from stored events, not a hard
  determinism guarantee.
- Tools run in-process only.
- Approvals are asynchronous: the run waits in `awaiting_approval` until a client resolves the
  approval over REST.
- Session history is linear.
- Tools default to `serial`; `parallel_read` and `parallel_isolated_write` tools run concurrently.
- Concurrent tool groups are inferred by the scheduler from each tool's concurrency class, not
  declared in the workflow.

Still open:
- What is the storage strategy for large artifacts? (Today they are stored inline in PostgreSQL.)
- What does resuming a run after a restart replay: the whole interrupted task, or from the last
  completed step? This decides whether steps need stored records.

## 18. First Implementation Milestones

The first implementation is viable when the framework can:
- [x] Accept a human request through REST.
- [x] Start a run and stream events through SSE.
- [x] Spawn sub-agents sequentially and concurrently.
- [x] Persist all major run events in PostgreSQL.
- [x] Call native Go tools.
- [ ] Call MCP tools.
- [x] Execute safe tool calls concurrently with cancellation.
- [ ] Bound concurrent tool calls with limits.
- [x] Enforce limits and stop runaway runs.
- [x] Replay a failed run.
- [ ] Resume a failed run.
- [ ] Produce observable logs, metrics, and traceable diagnostics.
- [x] Pass acceptance and integration test suites.
