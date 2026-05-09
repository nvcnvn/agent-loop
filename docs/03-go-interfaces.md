# Go Interfaces

These interfaces define the first development contracts. They intentionally separate runtime orchestration from persistence, model providers, tools, policy, and delivery.

Package names are suggestions:
- `core`: shared domain types and IDs
- `runtime`: orchestration, scheduling, replay
- `store`: persistence ports
- `model`: model provider ports
- `tool`: native and MCP tool ports
- `policy`: authorization, budgets, approvals
- `api`: REST/SSE adapters

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

type Limits struct {
    MaxDepth          int
    MaxSubAgents      int
    MaxConcurrent     int
    MaxToolCalls      int
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
```

## Runtime Orchestration

```go
package runtime

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

type Orchestrator interface {
    StartRun(ctx context.Context, input core.RunInput) (core.RunHandle, error)
    ResumeRun(ctx context.Context, tenantID core.TenantID, runID core.RunID) error
    CancelRun(ctx context.Context, tenantID core.TenantID, runID core.RunID, reason string) error
    ReplayRun(ctx context.Context, request ReplayRequest) (ReplayResult, error)
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

type ReplayEngine interface {
    BuildReplayPlan(ctx context.Context, request ReplayRequest) (ReplayPlan, error)
    ExecuteReplay(ctx context.Context, plan ReplayPlan) (ReplayResult, error)
}

type Snapshotter interface {
    ShouldSnapshot(ctx context.Context, runID core.RunID) (bool, error)
    CreateSnapshot(ctx context.Context, request SnapshotRequest) (SnapshotResult, error)
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

type EventStore interface {
    Append(ctx context.Context, events []core.Event) error
    ListRunEvents(ctx context.Context, tenantID core.TenantID, runID core.RunID, afterSeq int64, limit int) ([]core.Event, error)
    ListSessionEvents(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID, afterSeq int64, limit int) ([]core.Event, error)
    LoadReplayEvents(ctx context.Context, tenantID core.TenantID, runID core.RunID) ([]core.Event, error)
}

type SessionStore interface {
    CreateSession(ctx context.Context, session Session) (core.SessionID, error)
    GetSession(ctx context.Context, tenantID core.TenantID, sessionID core.SessionID) (Session, error)
    AddMessageProjection(ctx context.Context, message MessageProjection) error
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

type ArtifactStore interface {
    PutArtifact(ctx context.Context, artifact Artifact) (ArtifactRef, error)
    GetArtifact(ctx context.Context, tenantID core.TenantID, artifactID string) (Artifact, error)
    ListRunArtifacts(ctx context.Context, tenantID core.TenantID, runID core.RunID) ([]ArtifactRef, error)
}

type MemoryStore interface {
    WriteMemory(ctx context.Context, item MemoryItem) error
    SearchMemory(ctx context.Context, query MemoryQuery) ([]MemoryMatch, error)
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
    StreamChat(ctx context.Context, request ChatRequest) (ChatStream, error)
    Embed(ctx context.Context, request EmbedRequest) (EmbedResponse, error)
}

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
    TokenAccounting  bool
}

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

type ChatStream interface {
    Recv() (StreamEvent, error)
    Close() error
}
```

## Tool Contract

```go
package tool

import (
    "context"
    "encoding/json"

    "github.com/nvcnvn/agent-loop/core"
)

type Registry interface {
    RegisterNative(def Definition, exec Executor) error
    Discover(ctx context.Context, tenantID core.TenantID) ([]Definition, error)
    Get(ctx context.Context, tenantID core.TenantID, name string, version string) (Definition, Executor, error)
}

type Executor interface {
    Execute(ctx context.Context, call Call) (Result, error)
}

type Definition struct {
    ID               core.ToolID
    Name             string
    Version          string
    Description      string
    InputSchema      json.RawMessage
    OutputSchema     json.RawMessage
    Idempotency      IdempotencyHint
    SideEffect       SideEffectLevel
    Concurrency      ConcurrencyClass
    RequiresApproval bool
}

type Call struct {
    TenantID       core.TenantID
    RunID          core.RunID
    TaskID         *core.TaskID
    StepID         core.StepID
    ToolID         core.ToolID
    Input          json.RawMessage
    IdempotencyKey string
    Metadata       map[string]any
}

type Result struct {
    Output   json.RawMessage
    Metadata map[string]any
}

type MCPClient interface {
    Connect(ctx context.Context, server ServerConfig) error
    DiscoverTools(ctx context.Context) ([]Definition, error)
    Invoke(ctx context.Context, call Call) (Result, error)
    Close(ctx context.Context) error
}
```

## Policy, Budget, and Approval

```go
package policy

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
    "github.com/nvcnvn/agent-loop/tool"
)

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
    Tool     tool.Definition
    Input    []byte
}
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

```go
package observe

import (
    "context"

    "github.com/nvcnvn/agent-loop/core"
)

type Recorder interface {
    RunStarted(ctx context.Context, runID core.RunID, attrs map[string]any)
    RunEnded(ctx context.Context, runID core.RunID, status core.RunStatus, attrs map[string]any)
    ModelCall(ctx context.Context, attrs ModelCallAttrs)
    ToolCall(ctx context.Context, attrs ToolCallAttrs)
    BudgetUsage(ctx context.Context, tenantID core.TenantID, usage core.Usage)
    Error(ctx context.Context, err error, attrs map[string]any)
}
```

## Implementation Notes

- All interfaces accept `context.Context`; cancellation, deadlines, tenant identity, trace IDs, and auth claims should flow through it.
- Provider responses should store raw request/response envelopes for replay, with redaction policy applied before logs.
- Store interfaces should be implemented with optimistic locking or transactional state transitions, not blind updates.
- Public API handlers should depend on interfaces above, never on PostgreSQL or Ollama directly.
