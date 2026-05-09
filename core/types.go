// Package core defines shared domain types used across the agent-loop runtime.
//
// These types intentionally use string aliases for IDs rather than newtypes
// over uuid.UUID so they remain easy to marshal in JSON envelopes, REST
// responses and SSE payloads. Conversions live in this package.
package core

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type (
	TenantID    string
	PrincipalID string
	SessionID   string
	RunID       string
	TaskID      string
	StepID      string
	EventID     string
	AgentID     string
	ToolID      string
	ApprovalID  string
)

// DefaultTenant is used when running in single-tenant mode.
const DefaultTenant TenantID = "00000000-0000-0000-0000-000000000001"

// DefaultPrincipal is used when no authenticated principal is provided.
const DefaultPrincipal PrincipalID = "00000000-0000-0000-0000-000000000002"

// NewID returns a new uuidv7-shaped string. We use uuid.NewV7 when available
// and fall back to uuid.New (v4) on error.
func NewID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// RunStatus values mirror docs/01-domain-model.md.
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

func (s RunStatus) Terminal() bool {
	switch s {
	case RunCanceled, RunSucceeded, RunFailed, RunTimedOut:
		return true
	}
	return false
}

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

type Visibility string

const (
	VisibilityPublic          Visibility = "public"
	VisibilityParticipant     Visibility = "participant"
	VisibilityInternal        Visibility = "internal"
	VisibilityHiddenReasoning Visibility = "hidden_reasoning"
	VisibilityAudit           Visibility = "audit"
)

// Standardized event type names. Unknown event types are allowed; these are
// the ones the runtime emits itself.
const (
	EvHumanMessage    = "human_message_created"
	EvAgentMessage    = "agent_message_created"
	EvAgentTrace      = "agent_trace_recorded"
	EvSystem          = "system_event_recorded"
	EvModelStarted    = "model_call_started"
	EvModelCompleted  = "model_call_completed"
	EvToolScheduled   = "tool_call_scheduled"
	EvToolStarted     = "tool_call_started"
	EvToolCompleted   = "tool_call_completed"
	EvApprovalReq     = "approval_requested"
	EvApprovalRes     = "approval_resolved"
	EvSnapshot        = "snapshot_created"
	EvArtifact        = "artifact_created"
	EvTaskSpawned     = "task_spawned"
	EvTaskCompleted   = "task_completed"
	EvRunStateChanged = "run_state_changed"
	EvError           = "error_recorded"
)

// Event is the canonical, append-only history record.
type Event struct {
	ID              EventID         `json:"id"`
	TenantID        TenantID        `json:"tenant_id"`
	SessionID       SessionID       `json:"session_id"`
	RunID           RunID           `json:"run_id,omitempty"`
	Sequence        int64           `json:"sequence"`
	SessionSequence int64           `json:"session_sequence"`
	Type            string          `json:"type"`
	Visibility      Visibility      `json:"visibility"`
	ActorID         PrincipalID     `json:"actor_id,omitempty"`
	TaskID          TaskID          `json:"task_id,omitempty"`
	StepID          StepID          `json:"step_id,omitempty"`
	ParentEventID   EventID         `json:"parent_event_id,omitempty"`
	CorrelationID   string          `json:"correlation_id"`
	CausationID     string          `json:"causation_id,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	HiddenPayload   json.RawMessage `json:"hidden_payload,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
	RecordedAt      time.Time       `json:"recorded_at"`
}

// Limits mirrors §9.2 budget enforcement requirements.
type Limits struct {
	MaxDepth            int           `json:"max_depth,omitempty"`
	MaxSubAgents        int           `json:"max_sub_agents,omitempty"`
	MaxConcurrent       int           `json:"max_concurrent,omitempty"`
	MaxToolCalls        int           `json:"max_tool_calls,omitempty"`
	MaxModelCalls       int           `json:"max_model_calls,omitempty"`
	MaxRuntime          time.Duration `json:"max_runtime,omitempty"`
	MaxPromptTokens     int64         `json:"max_prompt_tokens,omitempty"`
	MaxCompletionTokens int64         `json:"max_completion_tokens,omitempty"`
	MaxCostUnits        int64         `json:"max_cost_units,omitempty"`
}

// Defaults applies sensible non-zero defaults to unset fields.
func (l Limits) Defaults() Limits {
	if l.MaxDepth == 0 {
		l.MaxDepth = 4
	}
	if l.MaxSubAgents == 0 {
		l.MaxSubAgents = 16
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = 4
	}
	if l.MaxToolCalls == 0 {
		l.MaxToolCalls = 64
	}
	if l.MaxModelCalls == 0 {
		l.MaxModelCalls = 64
	}
	if l.MaxRuntime == 0 {
		l.MaxRuntime = 5 * time.Minute
	}
	return l
}

type Usage struct {
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	CachedTokens     int64         `json:"cached_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	CostUnits        int64         `json:"cost_units"`
	ModelCalls       int64         `json:"model_calls"`
	ToolCalls        int64         `json:"tool_calls"`
	SubAgents        int64         `json:"sub_agents"`
	Runtime          time.Duration `json:"runtime"`
}

func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.CachedTokens += o.CachedTokens
	u.TotalTokens += o.TotalTokens
	u.CostUnits += o.CostUnits
	u.ModelCalls += o.ModelCalls
	u.ToolCalls += o.ToolCalls
	u.SubAgents += o.SubAgents
	u.Runtime += o.Runtime
}

// ErrBudgetExceeded is returned by the budget manager when a reservation
// or commit would exceed the configured limits.
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrPolicyDenied is returned by the policy engine.
var ErrPolicyDenied = errors.New("policy denied")

// ErrApprovalRequired is returned when a step needs human approval.
var ErrApprovalRequired = errors.New("approval required")

// ErrNotFound is returned by stores.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned for idempotency / optimistic concurrency conflicts.
var ErrConflict = errors.New("conflict")
