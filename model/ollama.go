package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nvcnvn/agent-loop/core"
)

// OllamaProvider implements Provider using the local Ollama HTTP API
// (https://github.com/ollama/ollama). It speaks the /api/chat endpoint.
type OllamaProvider struct {
	BaseURL string
	HTTP    *http.Client
}

// NewOllamaProvider returns a provider pointed at baseURL (e.g.
// "http://localhost:11434"). If client is nil a default with a 60s timeout
// is used.
func NewOllamaProvider(baseURL string, client *http.Client) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &OllamaProvider{BaseURL: baseURL, HTTP: client}
}

func (o *OllamaProvider) Name() string { return "ollama" }

func (o *OllamaProvider) Capabilities(_ context.Context, _ string) (Capabilities, error) {
	// Ollama supports tool-calling on a model-by-model basis. We advertise
	// the conservative set; callers can override per model.
	return Capabilities{Streaming: true, ToolCalling: true, TokenAccounting: true}, nil
}

type ollamaChatRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []map[string]any `json:"tools,omitempty"`
	Options  map[string]any   `json:"options,omitempty"`
	Format   any              `json:"format,omitempty"`
}

type ollamaChatResponse struct {
	Model           string `json:"model"`
	CreatedAt       string `json:"created_at"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
	Message         struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []map[string]any `json:"tool_calls,omitempty"`
	} `json:"message"`
}

func (o *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				var args any
				_ = json.Unmarshal(tc.Input, &args)
				tcs = append(tcs, map[string]any{
					"function": map[string]any{"name": tc.Name, "arguments": args},
				})
			}
			entry["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, entry)
	}

	tools := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		var schema any
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema)
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}

	body := ollamaChatRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   false,
		Tools:    tools,
		Options:  req.Parameters,
	}
	if len(req.ResponseSchema) > 0 {
		var schema any
		_ = json.Unmarshal(req.ResponseSchema, &schema)
		body.Format = schema
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(raw))
	}

	var oResp ollamaChatResponse
	if err := json.Unmarshal(raw, &oResp); err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: decode: %w", err)
	}

	calls := make([]ToolCall, 0, len(oResp.Message.ToolCalls))
	for i, tc := range oResp.Message.ToolCalls {
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		argsRaw, _ := json.Marshal(fn["arguments"])
		calls = append(calls, ToolCall{
			ID:    fmt.Sprintf("call_%d", i),
			Name:  name,
			Input: argsRaw,
		})
	}

	usage := core.Usage{
		PromptTokens:     oResp.PromptEvalCount,
		CompletionTokens: oResp.EvalCount,
		TotalTokens:      oResp.PromptEvalCount + oResp.EvalCount,
		ModelCalls:       1,
	}
	return ChatResponse{
		Message:   Message{Role: RoleAssistant, Content: oResp.Message.Content, ToolCalls: calls},
		ToolCalls: calls,
		Usage:     usage,
		Raw:       raw,
	}, nil
}
