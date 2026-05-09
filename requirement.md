# Agent Loop Requirements

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
- Spawn sub-agents from a parent agent.
- Support sequential execution, concurrent execution, and mixed graph-style execution.
- Allow workflow templates such as:
  - requirements -> code -> review -> test
  - planner -> multiple research agents in parallel -> synthesizer
- Allow an agent to hand work back to a previous stage when validation fails.
- Allow system-level limits on recursion depth, total spawned agents, concurrency, tokens, runtime, and tool calls.
- Allow per-workflow limits that override defaults within safe bounds.

The framework should define a clear execution model:
- `session`: long-lived collaboration container.
- `run`: one execution attempt inside a session.
- `agent`: a configured reasoning unit with model, tools, and policy.
- `task`: a unit of delegated work assigned to an agent.
- `step`: one atomic action within a run, such as model response, tool call, approval wait, or sub-agent spawn.

### 4.2 Workflow Definition

The framework must support two ways to define behavior:
- Dynamic delegation at runtime by an agent.
- Predefined workflow templates declared in configuration.

Workflow definitions should support:
- Agent role and instruction.
- Allowed tools.
- Allowed child agents.
- Retry policy.
- Timeout and deadline.
- Stop conditions.
- Human approval gates.
- Input and output schema.

## 5. State, Persistence, and Replay

### 5.1 Session Model

A session is a shared container that may include:
- Multiple human users.
- Multiple agents.
- Multiple runs over time.
- Full event history.

Each session must preserve:
- Ordered message history.
- Agent state transitions.
- Tool invocations and results.
- Workflow graph and parent-child relationships.
- Artifacts produced during execution.

### 5.2 Message Model

Support at least these message and event types:
- Human message.
- Agent visible message.
- Agent internal reasoning or hidden trace.
- System event.
- Tool call.
- Tool result.
- Approval request and approval result.
- Snapshot.
- Error event.

Requirements:
- Hidden reasoning must be stored separately from user-visible messages.
- Visibility rules must be explicit and configurable.
- Every event must include timestamps, actor identity, correlation IDs, and parent references.

### 5.3 Snapshot and Compaction

Support snapshots that summarize earlier context to control token growth.

Requirements:
- Trigger snapshots by message count, token count, or workflow boundary.
- Preserve the raw history even when summarized.
- Make snapshot generation auditable.
- Allow replay from raw history or from snapshot plus tail events.

### 5.4 Resumability and Replay

The framework must:
- Resume interrupted runs.
- Retry failed steps safely.
- Replay a run for debugging.
- Support deterministic replay mode where possible by storing model inputs, tool inputs, outputs, and configuration.

## 6. Tool Calling

### 6.1 Native Go Tools

Support tools implemented as normal Go functions with a defined registration contract.

Requirements:
- Strongly typed input and output schema.
- Validation before execution.
- Context propagation for timeout and cancellation.
- Structured error handling.
- Tool metadata: name, description, version, idempotency hint, side-effect level.

### 6.2 MCP Tools

Support MCP-based tools and remote MCP servers.

Requirements:
- Tool discovery.
- Schema translation into the framework's internal tool model.
- Authentication and connection lifecycle handling.
- Timeout, retry, and circuit-breaker support.

### 6.3 Tool Policy Layer

The framework should support policy enforcement per agent or workflow:
- Allowlist and denylist of tools.
- Read-only vs mutating tool classes.
- Approval required before dangerous tools.
- Max tool calls per run.
- Max cost or time budget per tool.

### 6.4 Concurrent Tool Execution

The framework should support concurrent tool calls when the workflow and tool policy allow it.

Requirements:
- Allow an agent step to schedule multiple tool calls in parallel.
- Allow the scheduler to serialize tool calls when ordering is required.
- Mark each tool with concurrency characteristics such as:
  - safe for parallel read operations
  - safe for parallel isolated writes
  - requires exclusive execution
  - non-idempotent or externally side-effecting
- Support fan-out and fan-in patterns where multiple tool results are collected before the next reasoning step.
- Support partial failure handling for parallel tool groups: fail-fast, wait-all, quorum, or best-effort.
- Enforce concurrency limits globally, per tenant, per workflow, per agent, and per tool.
- Preserve deterministic event ordering in persisted run history even when execution is concurrent.
- Propagate cancellation, timeout, and budget exhaustion to all in-flight tool calls.

Design rule:
- Concurrent tool execution should be opt-in by policy, not assumed safe by default.

## 7. Model Abstraction Layer

Start with Ollama, but the API must be provider-agnostic.

Requirements:
- Unified interface for chat completion, streaming, tool calling, embeddings, and structured output.
- Provider capability detection.
- Model-specific parameter mapping.
- Retry and backoff.
- Token accounting for prompt, completion, cached, and total usage when available.

Future providers may include hosted APIs, but the first production implementation only needs one production-ready adapter.

## 8. API and Protocols

### 8.1 External API

Expose HTTP APIs for:
- Create session.
- Post human message.
- Start run.
- Resume run.
- List runs, events, and artifacts.
- Approve or reject gated actions.
- Subscribe to run events.

### 8.2 Real-Time Delivery

Support HTTP SSE for streaming updates.

SSE requirements:
- Ordered event delivery per run.
- Event IDs for resume.
- Heartbeats.
- Delivery acknowledgement strategy or reconnect-safe resume.
- Separation of public events and internal events.

### 8.3 Compatibility Targets

- Provide a clean REST API first.
- Keep room for future ADK-compatible inbound or outbound integration.
- Document which parts are stable public API versus internal runtime contracts.

## 9. Safety and Control

### 9.1 Loop and Stall Detection

The framework must detect runaway behavior such as:
- Repeated identical tool calls.
- Repeated near-identical messages.
- Excessive delegation without progress.
- Retry storms.
- Long idle waits.

Responses may include:
- Warn.
- Pause run.
- Request human approval.
- Fail run with diagnostic reason.

### 9.2 Budget Enforcement

Track and enforce:
- Token budget.
- Time budget.
- Cost budget.
- Tool-call budget.
- Sub-agent budget.

### 9.3 Human-in-the-Loop

Support approval checkpoints for:
- High-cost actions.
- External side effects.
- Dangerous tools.
- Final answer publication in selected workflows.

## 10. Observability and Operations

This is currently missing and should be treated as first-class.

Requirements:
- Structured logs.
- Traces across model calls, tool calls, and sub-agent boundaries.
- Metrics for latency, token use, cost, queue depth, failures, retries, and tool usage.
- Audit log for user actions and approvals.
- Run timeline view data model.

Operational requirements:
- Graceful shutdown.
- Job recovery after process restart.
- Health endpoints.
- Backpressure handling.
- Configurable worker pool and queueing model.

## 11. Security and Multi-Tenancy

This is another missing area that should be explicit.

Requirements:
- Tenant isolation for sessions, runs, tools, and artifacts.
- Authentication and authorization.
- Secret management for tool credentials.
- Redaction support for logs and traces.
- Data retention policy.
- Optional encryption at rest for sensitive fields.

If multi-tenancy is not part of the initial implementation, explicitly state single-tenant deployment as the initial scope.

## 12. Memory and Retrieval

Since PostgreSQL and pgvector are part of the stack, define memory behavior explicitly.

Requirements:
- Store long-term memory separate from transient conversation state.
- Support embeddings for retrieval over prior sessions, documents, and artifacts.
- Scope retrieval by tenant, project, session, or workflow.
- Allow memory write policies so not every message becomes permanent memory.
- Track provenance for retrieved memories.

## 13. Artifacts and Outputs

Agents will often produce outputs beyond messages.

Support artifact storage for:
- Files.
- Structured JSON outputs.
- Reports.
- Code patches.
- Test results.

Requirements:
- Artifact versioning.
- MIME type and metadata.
- Link artifacts to run, step, and producer.

## 14. Testing Strategy

Testing should be expanded beyond the current note.

Requirements:
- Unit tests for workflow engine, persistence, budget enforcement, and policy checks.
- Acceptance tests with mock LLM adapters.
- Integration tests with Ollama in a controlled test environment.
- Contract tests for tool registration and MCP integration.
- Replay tests using recorded runs.
- Concurrency tests for fan-out and cancellation.
- Failure injection tests for network loss, tool timeout, and process restart.

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
- Accept human requests through an API and stream real-time execution events.
- Execute workflows that combine planning, delegation, tool use, review, and retry.
- Spawn multiple agents concurrently and merge their outputs safely.
- Schedule multiple tool calls concurrently when policy permits.
- Persist enough state to resume, inspect, replay, and audit any run.
- Apply budgets, limits, and approval gates without relying on prompt discipline alone.
- Support long-term memory and retrieval with clear provenance.
- Expose operational telemetry sufficient for debugging and production monitoring.
- Remain provider-agnostic at the model layer and protocol-agnostic at the tool layer.
- Pass deterministic or replay-based test scenarios for important workflows.

## 17. Open Design Questions

These should be answered before implementation starts:
- Is the initial implementation single-tenant or multi-tenant?
- Are hidden agent reasoning traces persisted by default, optionally, or never?
- Should workflow definitions be code-first, config-first, or both?
- Is deterministic replay a hard requirement or a best-effort debugging feature?
- Will tools run in-process only, or also via remote workers?
- What approval model is required: synchronous blocking, asynchronous callback, or both?
- Do sessions need branching history, or only linear history?
- What is the storage strategy for large artifacts?
- Which tool classes are eligible for concurrent execution by default?
- Should concurrent tool groups be modeled explicitly in the workflow definition, or inferred by the scheduler?

## 18. First Implementation Milestones

The first implementation is viable when the framework can:
- Accept a human request through REST.
- Start a run and stream events through SSE.
- Spawn sub-agents sequentially and concurrently.
- Persist all major run events in PostgreSQL.
- Call native Go tools and MCP tools.
- Execute safe tool calls concurrently with limits and cancellation.
- Enforce limits and stop runaway runs.
- Resume or replay a failed run.
- Produce observable logs, metrics, and traceable diagnostics.
- Pass acceptance and integration test suites.