// Package ollama implements model.Provider against the Ollama /api/chat
// endpoint (https://github.com/ollama/ollama).
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
)

// Provider talks to one Ollama server.
type Provider struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a provider pointed at baseURL (default "http://localhost:11434")
// using client (default: 60s timeout).
func New(baseURL string, client *http.Client) *Provider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Provider{BaseURL: baseURL, HTTP: client}
}

func (p *Provider) Name() string { return "ollama" }

func (p *Provider) Capabilities(_ context.Context, _ string) (model.Capabilities, error) {
	// Tool calling is per model in Ollama; this is the conservative common set.
	return model.Capabilities{Streaming: true, ToolCalling: true, TokenAccounting: true}, nil
}

func (p *Provider) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	buf, err := encodeRequest(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return model.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return model.ChatResponse{}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return model.ChatResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return model.ChatResponse{}, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(raw))
	}
	return decodeResponse(raw)
}

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []message        `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []map[string]any `json:"tools,omitempty"`
	Options  map[string]any   `json:"options,omitempty"`
	Format   json.RawMessage  `json:"format,omitempty"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	PromptEvalCount int64   `json:"prompt_eval_count"`
	EvalCount       int64   `json:"eval_count"`
	Message         message `json:"message"`
}

func encodeRequest(req model.ChatRequest) ([]byte, error) {
	msgs := make([]message, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if m.Role == model.RoleTool {
			om.ToolName = m.Name
		}
		for _, tc := range m.ToolCalls {
			var c toolCall
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Input
			om.ToolCalls = append(om.ToolCalls, c)
		}
		msgs = append(msgs, om)
	}

	tools := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			},
		})
	}

	return json.Marshal(chatRequest{
		Model:    req.Model,
		Messages: msgs,
		Tools:    tools,
		Options:  req.Parameters,
		Format:   req.ResponseSchema,
	})
}

func decodeResponse(raw []byte) (model.ChatResponse, error) {
	var r chatResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.ChatResponse{}, fmt.Errorf("ollama: decode: %w", err)
	}
	calls := make([]model.ToolCall, 0, len(r.Message.ToolCalls))
	for i, tc := range r.Message.ToolCalls {
		input := tc.Function.Arguments
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage(`{}`)
		}
		calls = append(calls, model.ToolCall{
			ID:    fmt.Sprintf("call_%d", i),
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return model.ChatResponse{
		Message:   model.Message{Role: model.RoleAssistant, Content: r.Message.Content, ToolCalls: calls},
		ToolCalls: calls,
		Usage: core.Usage{
			PromptTokens:     r.PromptEvalCount,
			CompletionTokens: r.EvalCount,
			TotalTokens:      r.PromptEvalCount + r.EvalCount,
			ModelCalls:       1,
		},
		Raw: raw,
	}, nil
}
