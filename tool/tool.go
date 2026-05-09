// Package tool provides the native Go tool registry and tool metadata types.
//
// Tools are looked up by name. Concurrency class and side-effect level drive
// scheduling and policy decisions in the runtime.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nvcnvn/agent-loop/core"
)

// IdempotencyHint matches the values in the schema and domain model.
type IdempotencyHint string

const (
	IdemUnknown               IdempotencyHint = "unknown"
	IdemIdempotent            IdempotencyHint = "idempotent"
	IdemConditionalIdempotent IdempotencyHint = "conditionally_idempotent"
	IdemNonIdempotent         IdempotencyHint = "non_idempotent"
)

// SideEffectLevel matches the values in the schema and domain model.
type SideEffectLevel string

const (
	SideNone          SideEffectLevel = "none"
	SideReadExternal  SideEffectLevel = "read_external"
	SideWriteInternal SideEffectLevel = "write_internal"
	SideWriteExternal SideEffectLevel = "write_external"
	SideDangerous     SideEffectLevel = "dangerous"
)

// ConcurrencyClass matches the values in the schema and domain model.
type ConcurrencyClass string

const (
	ConcurrencySerial                ConcurrencyClass = "serial"
	ConcurrencyParallelRead          ConcurrencyClass = "parallel_read"
	ConcurrencyParallelIsolatedWrite ConcurrencyClass = "parallel_isolated_write"
	ConcurrencyExclusive             ConcurrencyClass = "exclusive"
	ConcurrencyExternalSideEffect    ConcurrencyClass = "external_side_effect"
)

// Definition describes a tool to the model and to the runtime.
type Definition struct {
	ID               core.ToolID      `json:"id,omitempty"`
	Name             string           `json:"name"`
	Version          string           `json:"version"`
	Description      string           `json:"description"`
	InputSchema      json.RawMessage  `json:"input_schema"`
	OutputSchema     json.RawMessage  `json:"output_schema"`
	Idempotency      IdempotencyHint  `json:"idempotency"`
	SideEffect       SideEffectLevel  `json:"side_effect"`
	Concurrency      ConcurrencyClass `json:"concurrency"`
	RequiresApproval bool             `json:"requires_approval"`
}

// Call is one tool invocation request handed to an Executor.
type Call struct {
	TenantID       core.TenantID
	RunID          core.RunID
	TaskID         core.TaskID
	StepID         core.StepID
	Tool           Definition
	Input          json.RawMessage
	IdempotencyKey string
	Metadata       map[string]any
}

// Result is the structured output of a tool invocation.
type Result struct {
	Output   json.RawMessage
	Metadata map[string]any
}

// Executor performs the actual work of a tool.
type Executor interface {
	Execute(ctx context.Context, call Call) (Result, error)
}

// ExecutorFunc adapts a function to the Executor interface.
type ExecutorFunc func(ctx context.Context, call Call) (Result, error)

func (f ExecutorFunc) Execute(ctx context.Context, c Call) (Result, error) { return f(ctx, c) }

// Registry holds the set of native tools available to a tenant. The simple
// in-memory implementation is shared across tenants in single-tenant mode.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registered
}

type registered struct {
	def  Definition
	exec Executor
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]registered{}}
}

// Register adds (or replaces) a tool by name.
func (r *Registry) Register(def Definition, exec Executor) error {
	if def.Name == "" {
		return fmt.Errorf("tool: name required")
	}
	if exec == nil {
		return fmt.Errorf("tool: executor required")
	}
	if def.ID == "" {
		def.ID = core.ToolID(core.NewID())
	}
	if def.Version == "" {
		def.Version = "1.0.0"
	}
	if def.Idempotency == "" {
		def.Idempotency = IdemUnknown
	}
	if def.SideEffect == "" {
		def.SideEffect = SideNone
	}
	if def.Concurrency == "" {
		def.Concurrency = ConcurrencySerial
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Name] = registered{def: def, exec: exec}
	return nil
}

func (r *Registry) Get(name string) (Definition, Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return Definition{}, nil, false
	}
	return t.def, t.exec, true
}

func (r *Registry) List() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.def)
	}
	return out
}
