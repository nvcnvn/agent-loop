// Package tool provides the native Go tool registry and executors.
//
// Tools are looked up by name. The core.ToolDefinition concurrency class and
// side-effect level drive scheduling and policy decisions in the runtime.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

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

// Result is the structured output of a tool invocation and the artifacts it produced.
type Result struct {
	Output    json.RawMessage
	Metadata  map[string]any
	Artifacts []Artifact
	CostUnits int64 // what the call cost, counted toward the run's and the tool's cost budgets
}

// ErrTransient marks a tool error worth retrying: the runtime retries calls
// that fail with it, or time out, when the tool is idempotent.
var ErrTransient = errors.New("transient tool failure")

// Artifact is a file or document a tool produced; the runtime stores it with
// the run, task and step of the call.
type Artifact struct {
	Name     string
	Kind     string // such as file, json, report, patch or test_result; empty means file
	MimeType string // empty means application/octet-stream
	Content  []byte
	Metadata map[string]any
}

// Executor performs the actual work of a tool.
type Executor interface {
	Execute(ctx context.Context, call Call) (Result, error)
}

// ExecutorFunc adapts a function to the Executor interface.
type ExecutorFunc func(ctx context.Context, call Call) (Result, error)

func (f ExecutorFunc) Execute(ctx context.Context, c Call) (Result, error) { return f(ctx, c) }

// Registry holds the native and MCP tools available to a tenant. The simple
// in-memory implementation is shared across tenants in single-tenant mode.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registered
}

type registered struct {
	def    core.ToolDefinition
	exec   Executor
	schema *jsonschema.Schema
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]registered{}}
}

// Register adds (or replaces) a tool by name. Its input schema must compile.
func (r *Registry) Register(def core.ToolDefinition, exec Executor) error {
	if def.Name == "" {
		return fmt.Errorf("tool: name required")
	}
	if exec == nil {
		return fmt.Errorf("tool: executor required")
	}
	var schema *jsonschema.Schema
	if len(def.InputSchema) > 0 {
		var err error
		if schema, err = CompileSchema(def.InputSchema); err != nil {
			return fmt.Errorf("tool %s: input schema: %w", def.Name, err)
		}
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
		def.SideEffect = core.SideDangerous
	}
	if def.Concurrency == "" {
		def.Concurrency = core.ConcurrencySerial
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Name] = registered{def: def, exec: exec, schema: schema}
	return nil
}

// Unregister removes a tool; calls already holding its executor finish.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
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

// CheckInput validates input against the named tool's input schema.
func (r *Registry) CheckInput(name string, input json.RawMessage) error {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown tool %s", name)
	}
	if t.schema == nil {
		return nil
	}
	return validateJSON(t.schema, input)
}

// CompileSchema compiles a JSON Schema that may not reference any other document.
func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	const url = "urn:agent-loop:schema"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// validateJSON checks that doc is a JSON document that satisfies s.
func validateJSON(s *jsonschema.Schema, doc []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("not a JSON document: %w", err)
	}
	return s.Validate(v)
}
