package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"unicode"

	"github.com/nvcnvn/agent-loop/core"
)

// ErrNoScript is returned when the mock has no scripted response left.
var ErrNoScript = errors.New("mock: no scripted response left")

// MockResponse is one scripted reply.
type MockResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     core.Usage
	Err       error
}

// MockProvider returns scripted responses in FIFO order. Calls beyond the
// scripted set return ErrNoScript so tests fail loudly.
type MockProvider struct {
	mu        sync.Mutex
	responses []MockResponse
	calls     []ChatRequest
}

func NewMockProvider(responses ...MockResponse) *MockProvider {
	return &MockProvider{responses: responses}
}

func (m *MockProvider) Name() string { return "mock" }

func (m *MockProvider) Capabilities(_ context.Context, _ string) (Capabilities, error) {
	return Capabilities{ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true, TokenAccounting: true, Vision: true}, nil
}

func (m *MockProvider) Push(r MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, r)
}

func (m *MockProvider) Calls() []ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatRequest, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *MockProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if len(m.responses) == 0 {
		return ChatResponse{}, fmt.Errorf("%w (after %d calls)", ErrNoScript, len(m.calls))
	}
	r := m.responses[0]
	m.responses = m.responses[1:]
	if r.Err != nil {
		return ChatResponse{}, r.Err
	}
	usage := r.Usage
	if usage.TotalTokens == 0 {
		usage.PromptTokens = int64(len(req.Messages) * 4)
		usage.CompletionTokens = int64(len(r.Content) / 4)
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.ModelCalls = 1
	}
	raw, _ := json.Marshal(map[string]any{"mock": true, "content": r.Content})
	return ChatResponse{
		Message:   Message{Role: RoleAssistant, Content: r.Content, ToolCalls: r.ToolCalls},
		ToolCalls: r.ToolCalls,
		Usage:     usage,
		Raw:       raw,
	}, nil
}

// ChatStream answers like Chat and streams the content one word at a time.
func (m *MockProvider) ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (ChatResponse, error) {
	resp, err := m.Chat(ctx, req)
	if err != nil {
		return resp, err
	}
	for _, w := range strings.SplitAfter(resp.Message.Content, " ") {
		if w != "" {
			onDelta(Delta{Content: w})
		}
	}
	return resp, nil
}

// Embed returns deterministic bag-of-words vectors: texts sharing words get
// similar vectors, so retrieval tests rank by real overlap.
func (m *MockProvider) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 64)
		for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%64]++
		}
		var norm float64
		for _, x := range v {
			norm += float64(x * x)
		}
		if norm > 0 {
			for j := range v {
				v[j] /= float32(math.Sqrt(norm))
			}
		}
		out[i] = v
	}
	return out, nil
}
