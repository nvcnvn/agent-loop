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
	"errors"

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
	Thinking   string     `json:"thinking,omitempty"` // the model's reasoning, kept out of public events
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Images     []Image    `json:"images,omitempty"`
}

// Image is an image a message shows the model.
type Image struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// ErrUnsupported is returned when a model lacks a capability a request needs.
var ErrUnsupported = errors.New("unsupported by the model")

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
	// Parameters uses canonical names (temperature, top_p, max_tokens, seed,
	// stop) that each provider maps to its own; Ollama passes others through.
	Parameters     map[string]any `json:"parameters,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
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
	Vision            bool
	TokenAccounting   bool
}

// Provider is the model provider port.
type Provider interface {
	Name() string
	Capabilities(ctx context.Context, model string) (Capabilities, error)
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Delta is a fragment of a reply as the model generates it.
type Delta struct {
	Content  string
	Thinking string
}

// Streamer is a Provider that reports a reply while generating it. ChatStream
// calls onDelta in order from the calling goroutine and returns the same
// response Chat would.
type Streamer interface {
	Provider
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (ChatResponse, error)
}
