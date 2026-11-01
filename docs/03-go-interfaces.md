# Go Interfaces

These interfaces define the first development contracts. They intentionally separate runtime orchestration from persistence, model providers, tools, policy, and delivery.

Package names are suggestions:
- `core`: shared domain types, IDs and tool definitions
- `runtime`: orchestration, scheduling, replay
- `store`: persistence ports; drivers in subpackages (`store/pg`)
- `model`: model provider ports, scripted mock, fixture record/replay; retry wrapper; adapters in subpackages (`model/ollama`, `model/openai`)
- `tool`: native and MCP tool registries and executors
- `policy`: authorization, budgets, approvals
- `api`: REST/SSE adapters
- `observe`: metrics

## Core Types

```go
package core

import (
    "context"
    "encoding/json"
    "time"

    "github.com/google/uuid"
)

type TenantID uuid.UUID
type PrincipalID uuid.UUID
type SessionID uuid.UUID
type RunID uuid.UUID
type TaskID uuid.UUID
type StepID uuid.UUID
type EventID uuid.UUID
type AgentID uuid.UUID
type ToolID uuid.UUID

type RunStatus string
const (
    RunQueued           RunStatus = "queued"
    RunRunning          RunStatus = "running"
    RunAwaitingApproval RunStatus = "awaiting_approval"
    RunPaused           RunStatus = "paused"
    RunCanceling        RunStatus = "canceling"
    RunCanceled         RunStatus = "canceled"
    RunSucceeded        RunStatus = "succeeded"
    RunFailed           RunStatus = "failed"
    RunTimedOut         RunStatus = "timed_out"
)

type Visibility string
const (
    VisibilityPublic          Visibility = "public"
    VisibilityParticipant     Visibility = "participant"
    VisibilityInternal        Visibility = "internal"
    VisibilityHiddenReasoning Visibility = "hidden_reasoning"
    VisibilityAudit           Visibility = "audit"
)

type Event struct {
    ID               EventID
    TenantID         TenantID
    SessionID        SessionID
    RunID            *RunID
    Sequence         int64
    SessionSequence  int64
    Type             string
    Visibility       Visibility
    ActorID          *PrincipalID
    TaskID           *TaskID
    StepID           *StepID
    ParentEventID    *EventID
    CorrelationID    uuid.UUID
    CausationID      *uuid.UUID
    Payload          json.RawMessage
    HiddenPayload    json.RawMessage
    OccurredAt       time.Time
    RecordedAt       time.Time
}

// Limits are requested per run. StartRun fills unset fields (Defaults) and
// clamps every field to the server's Orchestrator.MaxLimits (Cap).
type Limits struct {
    MaxDepth          int
    MaxSubAgents      int
    MaxConcurrent     int
    MaxToolCalls      int
    MaxModelCalls     int
    MaxRuntime        time.Duration
    MaxPromptTokens   int64
    MaxCompletionTokens int64
    MaxCostUnits      int64
}

type Usage struct {
    PromptTokens     int64
    CompletionTokens int64
    CachedTokens     int64
    TotalTokens      int64
    CostUnits        int64
    ToolCalls        int64
    ModelCalls       int64
    SubAgents        int64
    Runtime          time.Duration
}

type RunInput struct {
    TenantID     TenantID
    SessionID    SessionID
    PrincipalID  PrincipalID
    WorkflowID   *uuid.UUID
    AgentID      *AgentID
    Message      json.RawMessage
    Limits       Limits
    Metadata     map[string]any
}

type RunHandle struct {
    RunID  RunID
    Status RunStatus
}

// SystemPrincipal is the actor of the runtime's own events; AgentPrincipal(name)
// is an agent's, a UUID derived from its name.
const SystemPrincipal PrincipalID = "00000000-0000-0000-0000-000000000003"
func AgentPrincipal(name string) PrincipalID

type ToolDefinition struct {
    ID               ToolID
    Name             string
    Version          string
    Description      string
    InputSchema      json.RawMessage
    OutputSchema     json.RawMessage
    Idempotency      IdempotencyHint  // unknown | idempotent | conditionally_idempotent | non_idempotent
    SideEffect       SideEffectLevel  // none | read_external | write_internal | write_external | dangerous
    Concurrency      ConcurrencyClass // serial | parallel_read | parallel_isolated_write | exclusive | external_side_effect
    RequiresApproval bool
    Timeout          time.Duration // per call; zero means only the run's deadline applies
}
```

## Runtime Orchestration

Workflows and agents are the JSON-tagged structs `runtime.Workflow`, `runtime.Stage` and
`runtime.Agent`, whose fields are the contract in `docs/04-contracts-and-tests.md` §3 and §4.
`StartRunInput.Template` names one of `Orchestrator.Workflows` instead of passing a `Workflow`, and
`runtime.ParseConfig(data []byte) (runtime.Config, error)` decodes and checks a config file of agents
and workflow templates.

```go
package runtime

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

type Orchestrator interface {
    StartRun(ctx context.Context, input core.RunInput) (core.RunHandle, error)
    // Resume records by as the actor of the events that mark the resumption.
    Resume(ctx context.Context, tenantID core.TenantID, runID core.RunID, by core.PrincipalID) error
    // Cancel records by and reason in an audit event, then stops the run; a finished run gives
    // core.ErrConflict.
    Cancel(ctx context.Context, tenantID core.TenantID, runID core.RunID, by core.PrincipalID, reason string) error
    // ReExecute runs a finished run again as a child run, answering model and tool calls
    // from its recorded outputs, and waits for the copy's result.
    ReExecute(ctx context.Context, tenantID core.TenantID, runID core.RunID, by core.PrincipalID) (Result, error)
}

type Scheduler interface {
    EnqueueRun(ctx context.Context, runID core.RunID) error
    EnqueueTask(ctx context.Context, task Task) error
    ScheduleToolGroup(ctx context.Context, group ToolGroup) error
    LeaseWork(ctx context.Context, worker WorkerRef) (WorkItem, error)
    CompleteWork(ctx context.Context, result WorkResult) error
}

type AgentRunner interface {
    RunTask(ctx context.Context, task TaskContext) (TaskResult, error)
}

type StepExecutor interface {
    ExecuteStep(ctx context.Context, step StepContext) (StepResult, error)
}

// LoopResponse (Orchestrator.LoopResponse) is what a detected loop does: LoopFail (default),
// LoopPause (the run waits in paused for Resume or Cancel) or LoopApprove (a human decides).
type LoopResponse string

// Orchestrator.ApproveAbove (core.Usage): a run past that many total tokens or cost units asks a
// human once before its next model call.

// Compaction (Orchestrator.Compaction) summarizes session history when a run starts.
type Compaction struct {
    Agent       string // writes the summary; empty disables compaction
    MaxMessages int    // trigger: messages since the last summary
    MaxTokens   int64  // trigger: their estimated tokens (characters / 4)
    KeepRecent  int    // messages left verbatim after the summary
}
```

## Persistence Ports

```go
package store

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

type Tx interface {
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
}

type UnitOfWork interface {
    WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// EventStore allocates per-run and per-session sequences atomically. Subscribe delivers a run's
// events after afterSeq in order until ctx ends or the returned func is called.
type EventStore interface {
    Append(ctx context.Context, events []core.Event) ([]core.Event, error)
    ListRunEvents(ctx context.Context, tenantID core.TenantID, runID core.RunID, afterSeq int64, limit int) ([]core.Event, error)
    ListSessionEvents(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID, afterSeq int64, limit int) ([]core.Event, error)
    Subscribe(ctx context.Context, runID core.RunID, afterSeq int64) (<-chan core.Event, func())
}

// SessionStore keeps sessions, their participants and messages. CreateSession makes the creator
// the session's owner; ListSessions with a participant lists only the sessions it takes part in.
// storetest.Participants is the conformance check.
type SessionStore interface {
    CreateSession(ctx context.Context, session Session) (core.SessionID, error)
    GetSession(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID) (Session, error)
    ListSessions(ctx context.Context, tenantID core.TenantID, participant core.PrincipalID) ([]Session, error)
    PutParticipant(ctx context.Context, p Participant) error // adds one, or changes its role
    GetParticipant(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID, principal core.PrincipalID) (Participant, error)
    ListParticipants(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID) ([]Participant, error)
    RemoveParticipant(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID, principal core.PrincipalID) error
    AddMessage(ctx context.Context, message MessageProjection) error
    ListMessages(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID) ([]MessageProjection, error)
}

type RunStore interface {
    CreateRun(ctx context.Context, run Run) (core.RunID, error)
    GetRun(ctx context.Context, tenantID core.TenantID, runID core.RunID) (Run, error)
    UpdateRunStatus(ctx context.Context, transition RunTransition) error
    ListRunnableRuns(ctx context.Context, tenantID core.TenantID, limit int) ([]Run, error)
}

type TaskStore interface {
    CreateTask(ctx context.Context, task TaskRecord) (core.TaskID, error)
    UpdateTask(ctx context.Context, task TaskRecord) error
    ListRunTasks(ctx context.Context, tenantID core.TenantID, runID core.RunID) ([]TaskRecord, error)
}

// ApprovalStore keeps approval requests. ResolveApproval returns core.ErrConflict for an approval
// that is already resolved or has expired. ListApprovals pages through the approvals f selects in ID
// (creation) order. storetest.Approvals is the conformance check.
type ApprovalStore interface {
    CreateApproval(ctx context.Context, a Approval) (core.ApprovalID, error)
    GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (Approval, error)
    ResolveApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string, by core.PrincipalID, resolution json.RawMessage) error
    WaitForApproval(ctx context.Context, id core.ApprovalID) (Approval, error)
    ListApprovals(ctx context.Context, tenant core.TenantID, f ApprovalFilter) ([]Approval, error)
    CancelRunApprovals(ctx context.Context, tenant core.TenantID, run core.RunID) error
}

// ApprovalFilter: empty fields select all; Status "requested" leaves out expired approvals.
type ApprovalFilter struct {
    RunID       core.RunID
    Status      string
    After       core.ApprovalID
    Limit       int
    Participant core.PrincipalID // only runs in sessions it participates in
}

type ArtifactStore interface {
    PutArtifact(ctx context.Context, artifact Artifact) (string, error)
    GetArtifact(ctx context.Context, tenantID core.TenantID, artifactID string) (Artifact, error)
    ListRunArtifacts(ctx context.Context, tenantID core.TenantID, runID core.RunID) ([]Artifact, error)
}

// MemoryStore keeps long-term memory. SearchMemories returns the tenant's memories made with
// query.EmbedModel that are tenant-scoped or scoped to query.SessionID or query.Workflow,
// nearest first by cosine similarity (MemoryHit.Score). storetest.Memories is the conformance check.
type MemoryStore interface {
    PutMemory(ctx context.Context, m Memory) (string, error)
    SearchMemories(ctx context.Context, query MemoryQuery) ([]MemoryHit, error)
}

// Store is what runtime and api take; /readyz calls Ping.
type Store interface {
    Ping(ctx context.Context) error
    EventStore
    SessionStore
    RunStore
    TaskStore
    ApprovalStore
    ArtifactStore
    MemoryStore
}
```

## Model Provider Contract

```go
package model

import (
    "context"
    "encoding/json"

    "github.com/nvcnvn/agent-loop/core"
)

type Provider interface {
    Name() string
    Capabilities(ctx context.Context, model string) (Capabilities, error)
    Chat(ctx context.Context, request ChatRequest) (ChatResponse, error)
}

// Streamer is a Provider that reports the reply while generating it; both
// adapters and MockProvider implement it. The runtime streams when it can.
type Streamer interface {
    Provider
    ChatStream(ctx context.Context, request ChatRequest, onDelta func(Delta)) (ChatResponse, error)
}

type Delta struct {
    Content  string
    Thinking string
}

// Embedder turns texts into vectors; both adapters, Retry and MockProvider
// (deterministic bag-of-words vectors) implement it.
type Embedder interface {
    Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// StatusError is a non-2xx provider response. WithRetry wraps a Provider and
// retries transient errors (Transient), and a stream only before its first delta.
type StatusError struct {
    Provider   string
    Code       int
    Body       string
    RetryAfter time.Duration
}
func Transient(err error) bool
func WithRetry(p Provider) *Retry

type ToolCallMode string
const (
    ToolCallNone     ToolCallMode = "none"
    ToolCallAuto     ToolCallMode = "auto"
    ToolCallRequired ToolCallMode = "required"
)

type Capabilities struct {
    Streaming        bool
    ToolCalling      bool
    ParallelToolCalls bool
    StructuredOutput bool
    Embeddings       bool
    Vision           bool
    TokenAccounting  bool
}

type Message struct {
    Role       string
    Content    string
    Thinking   string // reasoning; hidden from public events
    Name       string
    ToolCallID string
    ToolCalls  []ToolCall
    Images     []Image
}

type Image struct {
    MimeType string
    Data     []byte
}

var ErrUnsupported error // a model lacks a capability the request needs

type ChatRequest struct {
    TenantID       core.TenantID
    RunID          core.RunID
    StepID         core.StepID
    Model          string
    Messages       []Message
    Tools          []ToolSpec
    ToolCallMode   ToolCallMode
    ResponseSchema json.RawMessage
    Parameters     map[string]any
    IdempotencyKey string
}

type ChatResponse struct {
    Message    Message
    ToolCalls  []ToolCall
    Usage      core.Usage
    Raw        json.RawMessage
}

// Recorder wraps a live Provider and saves its successful exchanges as a
// fixture. Replayer is a Provider that answers requests matching a recorded
// one (ignoring IDs and model name) and returns ErrNoFixture otherwise, so
// regression tests run on real model output without a model server.
type Recorder struct{ Provider }
func NewRecorder(p Provider) *Recorder
func (r *Recorder) Save(path string) error

type Replayer struct{}
func LoadReplayer(path string) (*Replayer, error)
func (r *Replayer) Remaining() int
```

## Tool Contract

```go
package tool

import (
    "context"
    "encoding/json"

    "github.com/nvcnvn/agent-loop/core"
)

// Registry holds the tools agents may call, by name. Register compiles the input
// schema and refuses one that is invalid or references another document.
type Registry struct{}

func NewRegistry() *Registry
func (r *Registry) Register(def core.ToolDefinition, exec Executor) error
func (r *Registry) Get(name string) (core.ToolDefinition, Executor, bool)
func (r *Registry) List() []core.ToolDefinition
func (r *Registry) CheckInput(name string, input json.RawMessage) error

func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error)

type Executor interface {
    Execute(ctx context.Context, call Call) (Result, error)
}

type Call struct {
    TenantID       core.TenantID
    RunID          core.RunID
    TaskID         core.TaskID
    StepID         core.StepID
    Tool           core.ToolDefinition
    Input          json.RawMessage
    IdempotencyKey string
    Metadata       map[string]any
}

type Result struct {
    Output    json.RawMessage
    Metadata  map[string]any
    Artifacts []Artifact // stored by the runtime with the call's run, task and step
}

type Artifact struct {
    Name     string
    Kind     string
    MimeType string
    Content  []byte
    Metadata map[string]any
}

```

MCP servers are adapted in `tool/mcp`; only `cmd/` and `scenario/` import it:

```go
package mcp

type Server struct {
    Name       string            // tools are registered as <Name>.<tool>
    Command    []string          // stdio, or
    URL        string            // streamable HTTP
    Headers    map[string]string // ${VAR} read from the environment
    Timeout    time.Duration
    MaxRetries int
    BreakAfter int
    BreakFor   time.Duration
}

var ErrCircuitOpen error

func New(cfg Server) (*Client, error)
func (c *Client) Register(ctx context.Context, reg *tool.Registry) ([]string, error)
func (c *Client) Close() error
```

## Policy, Budget, and Approval

```go
package policy

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

// Role is a principal's tenant role: admin, operator, member or viewer. SessionRole is its role in
// one session: owner, member or viewer. Access combines them; admins and operators may do
// everything in every session.
type Role string
type SessionRole string
type SessionAccess struct{ Read, Write, Manage bool }
func Access(role Role, participant SessionRole) SessionAccess

// Visibility maps each role to the event visibilities it reads; DefaultVisibility gives members
// and viewers public and participant, operators also internal and audit, admins everything.
type Visibility map[Role][]core.Visibility
func DefaultVisibility() Visibility

type Engine interface {
    AuthorizeRun(ctx context.Context, request RunAuthorization) (Decision, error)
    AuthorizeToolCall(ctx context.Context, request ToolAuthorization) (Decision, error)
    AuthorizeDelegation(ctx context.Context, request DelegationAuthorization) (Decision, error)
    AuthorizeMemoryWrite(ctx context.Context, request MemoryAuthorization) (Decision, error)
}

type BudgetManager interface {
    Reserve(ctx context.Context, request BudgetReservation) (Reservation, error)
    Commit(ctx context.Context, reservation Reservation, usage core.Usage) error
    Release(ctx context.Context, reservation Reservation, reason string) error
    CurrentUsage(ctx context.Context, tenantID core.TenantID, runID core.RunID) (core.Usage, error)
}

type ApprovalService interface {
    RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalHandle, error)
    ResolveApproval(ctx context.Context, decision ApprovalDecision) error
    AwaitApproval(ctx context.Context, handle ApprovalHandle) (ApprovalDecision, error)
}

type Decision struct {
    Allowed          bool
    RequiresApproval bool
    Reason           string
    EffectiveLimits  core.Limits
}

type ToolAuthorization struct {
    TenantID core.TenantID
    RunID    core.RunID
    AgentID  core.AgentID
    Tool     core.ToolDefinition
    Input    []byte
}
```

## Authentication

```go
package api

type Identity struct {
    Tenant    core.TenantID
    Principal core.PrincipalID
    Role      policy.Role
}

// Authenticator resolves the caller of a request to a non-public route; an error answers 401.
type Authenticator func(*http.Request) (Identity, error)

func APIKeys(keys map[string]Identity) Authenticator // Authorization: Bearer <key>; no role means member
func ParseAPIKeys(spec string) (map[string]Identity, error) // "[<role>:]<tenant>:<principal>:<key>,..."
func HeaderIdentity(r *http.Request) (Identity, error) // trusts X-Tenant-ID / X-Principal-ID / X-Role (default admin)

// Server.Visibility (policy.DefaultVisibility unless set) decides which events and messages each
// role reads.
```

## Real-Time Delivery

```go
package api

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

type EventPublisher interface {
    Publish(ctx context.Context, event core.Event) error
}

type EventSubscriber interface {
    SubscribeRun(ctx context.Context, tenantID core.TenantID, runID core.RunID, afterSeq int64, visibility []core.Visibility) (Subscription, error)
}

type Subscription interface {
    Events() <-chan core.Event
    Errors() <-chan error
    Close() error
}
```

## Observability

Logs go to the `log/slog` loggers in `runtime.Orchestrator.Log` and `api.Server.Log`. Metrics go to
an `*observe.Metrics` (a nil one records nothing) and are served at `GET /metrics` in the Prometheus
text format:

```go
package observe

type Metrics struct{ /* counters, histograms, gauges */ }

func New() *Metrics
func (m *Metrics) Add(name string, v float64, labels ...string)     // counter
func (m *Metrics) Observe(name string, v float64, labels ...string) // histogram, seconds buckets
func (m *Metrics) Gauge(name string, f func() float64)              // read at scrape
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

| Metric | Labels |
| --- | --- |
| `agentloop_runs_finished_total` | `status` |
| `agentloop_run_duration_seconds` | |
| `agentloop_runs_queued`, `agentloop_runs_executing` (gauges) | |
| `agentloop_model_calls_total` | `model`, `outcome` (`ok`, `error`) |
| `agentloop_model_call_duration_seconds` | `model` |
| `agentloop_tokens_total` | `model`, `type` (`prompt`, `completion`) |
| `agentloop_cost_units_total` | `model` |
| `agentloop_tool_calls_total` | `tool` (`unknown` for calls to unregistered tools), `outcome` (`ok`, `error`; refused calls are errors) |
| `agentloop_tool_call_duration_seconds` | `tool` |
| `agentloop_task_retries_total` | `agent` |
| `agentloop_http_request_duration_seconds` | `route` (mux pattern), `code` |

`runtime.Orchestrator.Tracer` (OpenTelemetry; `NewOrchestrator` takes the global provider, which
records nothing until one is set) records a span tree per run: `run <workflow>` > `stage <name>` >
`task <agent>` > `chat <model>` and `execute_tool <tool>`, plus `await_approval`. A delegated
task's span is a child of the caller's `execute_tool delegate` span. Spans carry the run, task and
step IDs, and model spans the token counts. `cmd/agent-loop` exports them over OTLP/HTTP when
`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set (the other standard
`OTEL_*` variables apply), and its run logs carry `trace_id`.

## Implementation Notes

- All interfaces accept `context.Context`; cancellation, deadlines, tenant identity, trace IDs, and auth claims should flow through it.
- Provider responses should store raw request/response envelopes for replay, with redaction policy applied before logs.
- Store interfaces should be implemented with optimistic locking or transactional state transitions, not blind updates.
- Public API handlers should depend on interfaces above, never on PostgreSQL or Ollama directly.
