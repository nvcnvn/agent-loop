// Package openai implements model.Provider against OpenAI-compatible
// /chat/completions endpoints (OpenAI, GLM, vLLM, Ollama's /v1, ...).
package openai

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
)

// Price is what a million tokens cost, in the deployment's cost units.
type Price struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	Output      float64 `json:"output"`
}

// Provider talks to one OpenAI-compatible endpoint.
type Provider struct {
	BaseURL string // up to and including /v1
	APIKey  string
	HTTP    *http.Client
	// Caps is what every model behind the endpoint is assumed to support;
	// the protocol has no capability discovery.
	Caps  model.Capabilities
	Price Price
	// Extra is merged into every request body, for vendor switches such as
	// GLM's {"thinking": {"type": "disabled"}}.
	Extra map[string]any
}

// New returns a provider for baseURL with tool calling, structured output
// and token accounting assumed, and a 120s client timeout.
func New(baseURL, apiKey string) *Provider {
	return &Provider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
		Caps:    model.Capabilities{ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true, TokenAccounting: true},
	}
}

func (p *Provider) Name() string { return "openai" }

func (p *Provider) Capabilities(context.Context, string) (model.Capabilities, error) {
	return p.Caps, nil
}

func (p *Provider) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	body, err := p.encodeRequest(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	raw, err := p.post(ctx, "/chat/completions", body)
	if err != nil {
		return model.ChatResponse{}, err
	}
	resp, err := decodeResponse(raw)
	if err != nil {
		return resp, err
	}
	restoreToolNames(&resp, req.Tools)
	answerInReasoning(&resp, req)
	resp.Usage.CostUnits = p.cost(resp.Usage)
	return resp, nil
}

// answerInReasoning moves a structured answer that the server's reasoning
// parser filed as reasoning back into the content. GLM 5.3 on GreenNode does
// this whenever response_format constrains the output from the first token.
func answerInReasoning(resp *model.ChatResponse, req model.ChatRequest) {
	m := &resp.Message
	if len(req.ResponseSchema) > 0 && m.Content == "" && len(m.ToolCalls) == 0 && json.Valid([]byte(m.Thinking)) {
		m.Content, m.Thinking = strings.TrimSpace(m.Thinking), ""
	}
}

// Embed calls /embeddings.
func (p *Provider) Embed(ctx context.Context, name string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": name, "input": texts})
	raw, err := p.post(ctx, "/embeddings", body)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("openai: embed: %w", err)
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("openai: embed: index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("openai: embed: no vector for text %d", i)
		}
	}
	return out, nil
}

// ChatStream streams the reply over server-sent events.
func (p *Provider) ChatStream(ctx context.Context, req model.ChatRequest, onDelta func(model.Delta)) (model.ChatResponse, error) {
	body, err := p.encodeRequest(req)
	if err != nil {
		return model.ChatResponse{}, err
	}
	body = append(body[:len(body)-1], `,"stream":true,"stream_options":{"include_usage":true}}`...)
	httpResp, err := p.do(ctx, "/chat/completions", body)
	if err != nil {
		return model.ChatResponse{}, err
	}
	defer httpResp.Body.Close()
	resp, err := readStream(httpResp.Body, onDelta)
	if err != nil {
		return resp, err
	}
	restoreToolNames(&resp, req.Tools)
	answerInReasoning(&resp, req)
	resp.Usage.CostUnits = p.cost(resp.Usage)
	return resp, nil
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

func readStream(r io.Reader, onDelta func(model.Delta)) (model.ChatResponse, error) {
	var (
		content, reasoning strings.Builder
		lead               leadingThink
		calls              = map[int]*toolCall{}
		u                  *usage
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return model.ChatResponse{}, fmt.Errorf("openai: stream: %w", err)
		}
		if c.Usage != nil {
			u = c.Usage
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			content.WriteString(d.Content)
			reasoning.WriteString(d.ReasoningContent)
			text, thought := lead.push(d.Content)
			if thought += d.ReasoningContent; text != "" || thought != "" {
				onDelta(model.Delta{Content: text, Thinking: thought})
			}
			for _, tc := range d.ToolCalls {
				acc, ok := calls[tc.Index]
				if !ok {
					acc = &toolCall{Index: tc.Index}
					calls[tc.Index] = acc
				}
				acc.ID = cmp.Or(acc.ID, tc.ID)
				acc.Function.Name = cmp.Or(acc.Function.Name, tc.Function.Name)
				acc.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return model.ChatResponse{}, fmt.Errorf("openai: stream: %w", err)
	}
	if rest := lead.flush(); rest != "" {
		onDelta(model.Delta{Content: rest})
	}
	ordered := make([]toolCall, 0, len(calls))
	for _, tc := range calls {
		ordered = append(ordered, *tc)
	}
	slices.SortFunc(ordered, func(a, b toolCall) int { return a.Index - b.Index })
	out := assemble(content.String(), reasoning.String(), ordered)
	if u != nil {
		out.Usage = u.core()
	}
	return out, nil
}

// leadingThink holds content deltas back until it is clear whether the reply
// opens with inline reasoning, so the deltas add up to the final content.
type leadingThink struct {
	held    strings.Builder
	decided bool
}

func (l *leadingThink) push(s string) (content, thinking string) {
	if l.decided {
		return s, ""
	}
	l.held.WriteString(s)
	t := strings.TrimLeft(l.held.String(), " \t\r\n")
	switch {
	case t == "" || strings.HasPrefix("<think>", t) || strings.HasPrefix("</think>", t):
		return "", ""
	case strings.HasPrefix(t, "<think>") || strings.HasPrefix(t, "</think>"):
		before, after, found := strings.Cut(t, "</think>")
		if !found {
			return "", ""
		}
		l.decided = true
		return strings.TrimLeft(after, " \t\r\n"), strings.TrimSpace(strings.TrimPrefix(before, "<think>"))
	default:
		l.decided = true
		return l.held.String(), ""
	}
}

func (l *leadingThink) flush() string {
	if l.decided {
		return ""
	}
	l.decided = true
	return l.held.String()
}

// wireName makes a tool name match ^[a-zA-Z0-9_-]+$, which strict servers
// (GreenNode, OpenAI) require; "math.add" becomes "math_add".
func wireName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name)
}

// restoreToolNames maps wire names in resp back to the request's tool names.
// ponytail: two tools differing only in punctuation collide; first one wins.
func restoreToolNames(resp *model.ChatResponse, tools []model.ToolSpec) {
	names := make(map[string]string, len(tools))
	for _, t := range tools {
		if _, taken := names[wireName(t.Name)]; !taken {
			names[wireName(t.Name)] = t.Name
		}
	}
	for i, tc := range resp.ToolCalls {
		if name, ok := names[tc.Name]; ok {
			resp.ToolCalls[i].Name = name
		}
	}
	resp.Message.ToolCalls = resp.ToolCalls
}

func (p *Provider) cost(u core.Usage) int64 {
	uncached := u.PromptTokens - u.CachedTokens
	return int64((float64(uncached)*p.Price.Input + float64(u.CachedTokens)*p.Price.CachedInput + float64(u.CompletionTokens)*p.Price.Output) / 1e6)
}

func (p *Provider) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	resp, err := p.do(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (p *Provider) do(ctx context.Context, path string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, model.NewStatusError("openai "+path, resp, raw)
	}
	return resp, nil
}

type message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// paramNames maps canonical ChatRequest.Parameters keys to this protocol's.
var paramNames = map[string]string{"temperature": "temperature", "top_p": "top_p", "max_tokens": "max_tokens", "seed": "seed", "stop": "stop"}

func (p *Provider) encodeRequest(req model.ChatRequest) ([]byte, error) {
	body := map[string]any{}
	for k, v := range p.Extra {
		body[k] = v
	}
	for k, v := range req.Parameters {
		if name, ok := paramNames[k]; ok {
			body[name] = v
		}
	}
	msgs := make([]message, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if len(m.Images) > 0 {
			parts := []part{{Type: "text", Text: m.Content}}
			for _, img := range m.Images {
				parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: "data:" + img.MimeType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}})
			}
			om.Content = parts
		}
		for i, tc := range m.ToolCalls {
			c := toolCall{Index: i, ID: tc.ID, Type: "function"}
			c.Function.Name, c.Function.Arguments = wireName(tc.Name), string(tc.Input)
			om.ToolCalls = append(om.ToolCalls, c)
		}
		msgs = append(msgs, om)
	}
	body["model"] = req.Model
	body["messages"] = msgs
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": wireName(t.Name), "description": t.Description, "parameters": t.InputSchema,
			}})
		}
		body["tools"] = tools
		if req.ToolCallMode != "" {
			body["tool_choice"] = req.ToolCallMode
		}
	}
	if len(req.ResponseSchema) > 0 {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "schema": req.ResponseSchema}}
	}
	return json.Marshal(body)
}

type usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u usage) core() core.Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return core.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CachedTokens: u.PromptTokensDetails.CachedTokens, TotalTokens: total, ModelCalls: 1}
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          *string    `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

func decodeResponse(raw []byte) (model.ChatResponse, error) {
	var r chatResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.ChatResponse{}, fmt.Errorf("openai: decode: %w", err)
	}
	if len(r.Choices) == 0 {
		return model.ChatResponse{}, fmt.Errorf("openai: response has no choices: %.200s", raw)
	}
	m := r.Choices[0].Message
	content := ""
	if m.Content != nil {
		content = *m.Content
	}
	out := assemble(content, m.ReasoningContent, m.ToolCalls)
	if r.Usage != nil {
		out.Usage = r.Usage.core()
	}
	out.Raw = raw
	return out, nil
}

// assemble builds the response message from content, reasoning and tool
// calls, as decoded whole or accumulated from stream deltas.
func assemble(content, reasoning string, calls []toolCall) model.ChatResponse {
	thinking, content := splitThinking(content)
	if reasoning != "" {
		thinking = reasoning
	}
	out := make([]model.ToolCall, 0, len(calls))
	for i, tc := range calls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out = append(out, model.ToolCall{ID: id, Name: tc.Function.Name, Input: arguments(tc.Function.Arguments)})
	}
	return model.ChatResponse{
		Message:   model.Message{Role: model.RoleAssistant, Content: content, Thinking: thinking, ToolCalls: out},
		ToolCalls: out,
		Usage:     core.Usage{ModelCalls: 1},
	}
}

// splitThinking separates reasoning a server left inline at the start of the
// content. GLM 5.3 prefixes "</think>" even with thinking disabled.
func splitThinking(content string) (thinking, rest string) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "<think>") && !strings.HasPrefix(trimmed, "</think>") {
		return "", content
	}
	before, after, found := strings.Cut(trimmed, "</think>")
	if !found {
		return "", content
	}
	return strings.TrimSpace(strings.TrimPrefix(before, "<think>")), strings.TrimSpace(after)
}

// arguments returns the model's tool arguments as JSON; malformed ones are
// passed on as a JSON string so input validation reports them to the model.
func arguments(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}
