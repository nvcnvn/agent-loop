// Package memory provides the memory.save and memory.search tools: long-term
// memory that agents write and retrieve by meaning, kept apart from session
// messages.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// Store is what the tools need from persistence.
type Store interface {
	store.RunStore
	store.MemoryStore
}

// Tools writes and searches memories with one embedding model. Nothing
// becomes memory unless an agent granted memory.save saves it, and then only
// at WriteScope: that pair is the write policy.
type Tools struct {
	Store    Store
	Embedder model.Embedder
	Model    string // embedding model
	// WriteScope is where memory.save writes; default session.
	WriteScope store.MemoryScope
	// MaxChars caps one saved memory; default 2000.
	MaxChars int
	// Limit is how many memories memory.search returns; default 5.
	Limit int
}

// Tool names.
const (
	SaveTool   = "memory.save"
	SearchTool = "memory.search"
)

// Register adds memory.save and memory.search to reg.
func (t Tools) Register(reg *tool.Registry) error {
	if t.WriteScope == "" {
		t.WriteScope = store.MemorySession
	}
	if t.MaxChars <= 0 {
		t.MaxChars = 2000
	}
	if t.Limit <= 0 {
		t.Limit = 5
	}
	if err := reg.Register(core.ToolDefinition{
		Name:        SaveTool,
		Description: "Remember a fact, preference or decision for later conversations. Save one self-contained statement per call.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"the statement to remember"}},"required":["content"]}`),
		SideEffect:  core.SideWriteInternal,
		Concurrency: core.ConcurrencyParallelIsolatedWrite,
	}, tool.ExecutorFunc(t.save)); err != nil {
		return err
	}
	return reg.Register(core.ToolDefinition{
		Name:        SearchTool,
		Description: "Search what was remembered in earlier conversations. Returns the closest memories with where they came from.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"what to look for"}},"required":["query"]}`),
		Idempotency: core.IdemIdempotent,
		SideEffect:  core.SideNone,
		Concurrency: core.ConcurrencyParallelRead,
	}, tool.ExecutorFunc(t.search))
}

// scope returns the run's session and workflow name.
func (t Tools) scope(ctx context.Context, c tool.Call) (core.SessionID, string, error) {
	run, err := t.Store.GetRun(ctx, c.TenantID, c.RunID)
	if err != nil {
		return "", "", err
	}
	var wf struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(run.Workflow, &wf)
	return run.SessionID, wf.Name, nil
}

func (t Tools) embed(ctx context.Context, text string) ([]float32, error) {
	v, err := t.Embedder.Embed(ctx, t.Model, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	return v[0], nil
}

func (t Tools) save(ctx context.Context, c tool.Call) (tool.Result, error) {
	var in struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tool.Result{}, err
	}
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		return tool.Result{}, errors.New("content is empty")
	}
	if len(in.Content) > t.MaxChars {
		return tool.Result{}, fmt.Errorf("content is %d characters; save at most %d", len(in.Content), t.MaxChars)
	}
	session, workflow, err := t.scope(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	vec, err := t.embed(ctx, in.Content)
	if err != nil {
		return tool.Result{}, err
	}
	m := store.Memory{
		TenantID:   c.TenantID,
		Scope:      t.WriteScope,
		Content:    in.Content,
		Embedding:  vec,
		EmbedModel: t.Model,
		Source:     store.MemorySource{RunID: c.RunID, SessionID: session, TaskID: c.TaskID, StepID: c.StepID},
	}
	switch t.WriteScope {
	case store.MemorySession:
		m.SessionID = session
	case store.MemoryWorkflow:
		m.Workflow = workflow
	}
	id, err := t.Store.PutMemory(ctx, m)
	if err != nil {
		return tool.Result{}, err
	}
	out, _ := json.Marshal(map[string]any{"saved": id, "scope": m.Scope})
	return tool.Result{Output: out}, nil
}

func (t Tools) search(ctx context.Context, c tool.Call) (tool.Result, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tool.Result{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return tool.Result{}, errors.New("query is empty")
	}
	session, workflow, err := t.scope(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	vec, err := t.embed(ctx, in.Query)
	if err != nil {
		return tool.Result{}, err
	}
	hits, err := t.Store.SearchMemories(ctx, store.MemoryQuery{
		TenantID: c.TenantID, Embedding: vec, EmbedModel: t.Model, SessionID: session, Workflow: workflow, Limit: t.Limit,
	})
	if err != nil {
		return tool.Result{}, err
	}
	type found struct {
		Content string             `json:"content"`
		Score   float64            `json:"score"`
		Scope   store.MemoryScope  `json:"scope"`
		Source  store.MemorySource `json:"source"`
		Saved   string             `json:"saved_at"`
	}
	memories := make([]found, 0, len(hits))
	for _, h := range hits {
		memories = append(memories, found{Content: h.Content, Score: float64(int(h.Score*1000)) / 1000, Scope: h.Scope, Source: h.Source, Saved: h.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")})
	}
	out, _ := json.Marshal(map[string]any{"memories": memories})
	return tool.Result{Output: out}, nil
}
