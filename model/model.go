// Package model defines the model provider port, the scripted MockProvider,
// and the fixture Recorder/Replayer.
//
// The runtime depends only on Provider; network adapters live in
// subpackages (model/ollama) so the runtime stays provider-agnostic per
// requirement §7.
package model

import (
	"context"
	"encoding/json"

	"github.com/nvcnvn/agent-loop/core"
)

// Role values used in messages exchanged with the provider.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is the canonical chat message exchanged with the provider.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolSpec is the schema-shaped tool description we advertise to providers.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall is a structured tool invocation requested by the model.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ChatRequest is the unified chat completion request.
type ChatRequest struct {
	TenantID       core.TenantID   `json:"tenant_id,omitempty"`
	RunID          core.RunID      `json:"run_id,omitempty"`
	StepID         core.StepID     `json:"step_id,omitempty"`
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolSpec      `json:"tools,omitempty"`
	ToolCallMode   string          `json:"tool_call_mode,omitempty"` // "auto" | "required" | "none"
	ResponseSchema json.RawMessage `json:"response_schema,omitempty"`
	Parameters     map[string]any  `json:"parameters,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// ChatResponse is the unified chat completion response.
type ChatResponse struct {
	Message   Message         `json:"message"`
	ToolCalls []ToolCall      `json:"tool_calls,omitempty"`
	Usage     core.Usage      `json:"usage"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Capabilities lets agents fail fast if a feature is unsupported.
type Capabilities struct {
	Streaming         bool
	ToolCalling       bool
	ParallelToolCalls bool
	StructuredOutput  bool
	Embeddings        bool
	TokenAccounting   bool
}

// Provider is the model provider port.
type Provider interface {
	Name() string
	Capabilities(ctx context.Context, model string) (Capabilities, error)
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
