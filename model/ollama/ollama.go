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
	"slices"
	"sync"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
)

// Provider talks to one Ollama server.
type Provider struct {
	BaseURL string
	HTTP    *http.Client

	caps sync.Map // model name -> model.Capabilities
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

// Capabilities asks the server what the model can do, once per model.
func (p *Provider) Capabilities(ctx context.Context, name string) (model.Capabilities, error) {
	if c, ok := p.caps.Load(name); ok {
		return c.(model.Capabilities), nil
	}
	body, _ := json.Marshal(map[string]string{"model": name})
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := p.post(ctx, "/api/show", body, &show); err != nil {
		return model.Capabilities{}, err
	}
	c := model.Capabilities{
		Streaming:        true,
		TokenAccounting:  true,
		StructuredOutput: true,
		ToolCalling:      slices.Contains(show.Capabilities, "tools"),
		Vision:           slices.Contains(show.Capabilities, "vision"),
		Embeddings:       slices.Contains(show.Capabilities, "embedding"),
	}
	p.caps.Store(name, c)
	return c, nil
}

func (p *Provider) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	buf, err := encodeRequest(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	var raw json.RawMessage
	if err := p.post(ctx, "/api/chat", buf, &raw); err != nil {
		return model.ChatResponse{}, err
	}
	return decodeResponse(raw)
}

// Embed calls /api/embed.
func (p *Provider) Embed(ctx context.Context, name string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": name, "input": texts})
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := p.post(ctx, "/api/embed", body, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama: embed: %d vectors for %d texts", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// ChatStream streams the reply as Ollama's newline-delimited JSON chunks.
func (p *Provider) ChatStream(ctx context.Context, req model.ChatRequest, onDelta func(model.Delta)) (model.ChatResponse, error) {
	buf, err := encodeRequest(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	buf = bytes.Replace(buf, []byte(`"stream":false`), []byte(`"stream":true`), 1)
	resp, err := p.do(ctx, "/api/chat", buf)
	if err != nil {
		return model.ChatResponse{}, err
	}
	defer resp.Body.Close()
	var acc chatResponse
	dec := json.NewDecoder(resp.Body)
	for {
		var c chatResponse
		if err := dec.Decode(&c); err == io.EOF {
			break
		} else if err != nil {
			return model.ChatResponse{}, fmt.Errorf("ollama: stream: %w", err)
		}
		if c.Message.Content != "" || c.Message.Thinking != "" {
			onDelta(model.Delta{Content: c.Message.Content, Thinking: c.Message.Thinking})
		}
		acc.Message.Content += c.Message.Content
		acc.Message.Thinking += c.Message.Thinking
		acc.Message.ToolCalls = append(acc.Message.ToolCalls, c.Message.ToolCalls...)
		if c.Done {
			acc.PromptEvalCount, acc.EvalCount = c.PromptEvalCount, c.EvalCount
		}
	}
	raw, _ := json.Marshal(acc)
	return decodeResponse(raw)
}

func (p *Provider) do(ctx context.Context, path string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, model.NewStatusError("ollama "+path, resp, raw)
	}
	return resp, nil
}

func (p *Provider) post(ctx context.Context, path string, body []byte, out any) error {
	resp, err := p.do(ctx, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if r, ok := out.(*json.RawMessage); ok {
		*r = raw
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ollama: %s: decode: %w", path, err)
	}
	return nil
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
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Images     [][]byte   `json:"images,omitempty"`
}

type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Done            bool    `json:"done"`
	PromptEvalCount int64   `json:"prompt_eval_count"`
	EvalCount       int64   `json:"eval_count"`
	Message         message `json:"message"`
}

func encodeRequest(req model.ChatRequest) ([]byte, error) {
	msgs := make([]message, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := message{Role: m.Role, Content: m.Content, Thinking: m.Thinking, ToolCallID: m.ToolCallID}
		if m.Role == model.RoleTool {
			om.ToolName = m.Name
		}
		for _, img := range m.Images {
			om.Images = append(om.Images, img.Data)
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

	var options map[string]any
	for k, v := range req.Parameters {
		if options == nil {
			options = map[string]any{}
		}
		if k == "max_tokens" {
			k = "num_predict"
		}
		options[k] = v
	}
	return json.Marshal(chatRequest{
		Model:    req.Model,
		Messages: msgs,
		Tools:    tools,
		Options:  options,
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
		Message:   model.Message{Role: model.RoleAssistant, Content: r.Message.Content, Thinking: r.Message.Thinking, ToolCalls: calls},
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
