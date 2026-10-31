// Package tool provides the native Go tool registry and executors.
//
// Tools are looked up by name. The core.ToolDefinition concurrency class and
// side-effect level drive scheduling and policy decisions in the runtime.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nvcnvn/agent-loop/core"
)

// Call is one tool invocation request handed to an Executor.
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
	def  core.ToolDefinition
	exec Executor
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]registered{}}
}

// Register adds (or replaces) a tool by name.
func (r *Registry) Register(def core.ToolDefinition, exec Executor) error {
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
		def.Idempotency = core.IdemUnknown
	}
	if def.SideEffect == "" {
		def.SideEffect = core.SideNone
	}
	if def.Concurrency == "" {
		def.Concurrency = core.ConcurrencySerial
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Name] = registered{def: def, exec: exec}
	return nil
}

func (r *Registry) Get(name string) (core.ToolDefinition, Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return core.ToolDefinition{}, nil, false
	}
	return t.def, t.exec, true
}

func (r *Registry) List() []core.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]core.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.def)
	}
	return out
}
