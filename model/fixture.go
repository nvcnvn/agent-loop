package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrNoFixture is returned by Replayer when no recorded exchange matches a request.
var ErrNoFixture = errors.New("replay: no recorded response for request")

// Exchange is one recorded provider call.
type Exchange struct {
	Request  ChatRequest  `json:"request"`
	Response ChatResponse `json:"response"`
}

// Recorder wraps a Provider and keeps every successful exchange for Save.
type Recorder struct {
	Provider

	mu        sync.Mutex
	exchanges []Exchange
}

// NewRecorder returns a Recorder around p.
func NewRecorder(p Provider) *Recorder { return &Recorder{Provider: p} }

func (r *Recorder) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := r.Provider.Chat(ctx, req)
	if err != nil {
		return resp, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchanges = append(r.exchanges, Exchange{Request: withoutIDs(req), Response: resp})
	return resp, nil
}

// Save writes the recorded exchanges to path as a fixture LoadReplayer reads.
func (r *Recorder) Save(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, err := json.MarshalIndent(r.exchanges, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o644)
}

// Replayer answers each request with the recorded response to an identical
// request, ignoring IDs and model name. Identical requests are answered in
// recorded order, so concurrent callers need no fixed call order.
type Replayer struct {
	mu      sync.Mutex
	pending map[string][]ChatResponse
}

// LoadReplayer reads a fixture written by Recorder.Save.
func LoadReplayer(path string) (*Replayer, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exchanges []Exchange
	if err := json.Unmarshal(buf, &exchanges); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	r := &Replayer{pending: map[string][]ChatResponse{}}
	for _, ex := range exchanges {
		k, err := matchKey(ex.Request)
		if err != nil {
			return nil, err
		}
		r.pending[k] = append(r.pending[k], ex.Response)
	}
	return r, nil
}

func (r *Replayer) Name() string { return "replay" }

func (r *Replayer) Capabilities(_ context.Context, _ string) (Capabilities, error) {
	return Capabilities{ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true, TokenAccounting: true}, nil
}

func (r *Replayer) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	k, err := matchKey(req)
	if err != nil {
		return ChatResponse{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.pending[k]
	if len(q) == 0 {
		return ChatResponse{}, fmt.Errorf("%w: %s", ErrNoFixture, k)
	}
	r.pending[k] = q[1:]
	return q[0], nil
}

// Remaining reports how many recorded responses were never requested.
func (r *Replayer) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.pending {
		n += len(q)
	}
	return n
}

func withoutIDs(req ChatRequest) ChatRequest {
	req.TenantID, req.RunID, req.StepID, req.IdempotencyKey = "", "", "", ""
	return req
}

func matchKey(req ChatRequest) (string, error) {
	req = withoutIDs(req)
	req.Model = ""
	buf, err := json.Marshal(req)
	return string(buf), err
}
