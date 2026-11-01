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
	"unicode/utf8"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// Store is what the tools need from persistence.
type Store interface {
	store.RunStore
	store.MemoryStore
	GetSession(ctx context.Context, tenant core.TenantID, id core.SessionID) (store.Session, error)
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
	// MaxChars caps one saved memory, and each part of an ingested document, in bytes; default 2000.
	MaxChars int
	// MaxIngestBytes caps one ingested document; default 1 MiB.
	MaxIngestBytes int
	// Limit is how many memories memory.search returns; default 5.
	Limit int
}

// Tool names.
const (
	SaveTool   = "memory.save"
	SearchTool = "memory.search"
)

func (t Tools) withDefaults() Tools {
	if t.WriteScope == "" {
		t.WriteScope = store.MemorySession
	}
	if t.MaxChars <= 0 {
		t.MaxChars = 2000
	}
	if t.MaxIngestBytes <= 0 {
		t.MaxIngestBytes = 1 << 20
	}
	if t.Limit <= 0 {
		t.Limit = 5
	}
	return t
}

// Register adds memory.save and memory.search to reg.
func (t Tools) Register(reg *tool.Registry) error {
	t = t.withDefaults()
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

// where is what a call's run is part of, which scopes its memories.
type where struct {
	session  core.SessionID
	project  string
	workflow string
}

func (t Tools) scope(ctx context.Context, c tool.Call) (where, error) {
	run, err := t.Store.GetRun(ctx, c.TenantID, c.RunID)
	if err != nil {
		return where{}, err
	}
	sess, err := t.Store.GetSession(ctx, c.TenantID, run.SessionID)
	if err != nil {
		return where{}, err
	}
	var wf struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(run.Workflow, &wf)
	return where{session: run.SessionID, project: sess.Project, workflow: wf.Name}, nil
}

// scoped fills m's scope fields for scope, or fails when w has nothing to scope it to.
func scoped(m store.Memory, scope store.MemoryScope, w where) (store.Memory, error) {
	m.Scope = scope
	switch scope {
	case store.MemorySession:
		m.SessionID = w.session
	case store.MemoryProject:
		if w.project == "" {
			return m, fmt.Errorf("%w: this session belongs to no project, so there is no project memory to save to", store.ErrInvalidMemory)
		}
		m.Project = w.project
	case store.MemoryWorkflow:
		m.Workflow = w.workflow
	}
	return m, nil
}

func (t Tools) embed(ctx context.Context, text string) ([]float32, error) {
	v, err := t.Embedder.Embed(ctx, t.Model, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(v) != 1 {
		return nil, fmt.Errorf("embed: %d vectors for one text", len(v))
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
		return tool.Result{}, fmt.Errorf("content is %d bytes; save at most %d", len(in.Content), t.MaxChars)
	}
	w, err := t.scope(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	m, err := scoped(store.Memory{
		TenantID:   c.TenantID,
		Content:    in.Content,
		EmbedModel: t.Model,
		Source:     store.MemorySource{RunID: c.RunID, SessionID: w.session, TaskID: c.TaskID, StepID: c.StepID},
	}, t.WriteScope, w)
	if err != nil {
		return tool.Result{}, err
	}
	if m.Embedding, err = t.embed(ctx, in.Content); err != nil {
		return tool.Result{}, err
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
	w, err := t.scope(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	vec, err := t.embed(ctx, in.Query)
	if err != nil {
		return tool.Result{}, err
	}
	hits, err := t.Store.SearchMemories(ctx, store.MemoryQuery{
		TenantID: c.TenantID, Embedding: vec, EmbedModel: t.Model, SessionID: w.session, Project: w.project, Workflow: w.workflow, Limit: t.Limit,
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

// Ingest splits a document into parts of at most MaxChars bytes, at
// paragraph breaks where it can, and saves all of them, or none, as memories
// of session's at scope (session, project or tenant), with src and the part
// number as provenance.
func (t Tools) Ingest(ctx context.Context, tenant core.TenantID, session core.SessionID, scope store.MemoryScope, text string, src store.MemorySource) ([]string, error) {
	t = t.withDefaults()
	if scope != store.MemorySession && scope != store.MemoryProject && scope != store.MemoryTenant {
		return nil, fmt.Errorf("%w: ingest into session, project or tenant memory, not %q", store.ErrInvalidMemory, scope)
	}
	if len(text) > t.MaxIngestBytes {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", store.ErrMemoryTooLarge, len(text), t.MaxIngestBytes)
	}
	sess, err := t.Store.GetSession(ctx, tenant, session)
	if err != nil {
		return nil, err
	}
	src.SessionID = session
	template, err := scoped(store.Memory{TenantID: tenant, EmbedModel: t.Model}, scope, where{session: session, project: sess.Project})
	if err != nil {
		return nil, err
	}
	parts := split(strings.ReplaceAll(text, "\r\n", "\n"), t.MaxChars)
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: nothing to ingest", store.ErrInvalidMemory)
	}
	memories := make([]store.Memory, 0, len(parts))
	for start := 0; start < len(parts); start += embedBatch {
		batch := parts[start:min(start+embedBatch, len(parts))]
		vecs, err := t.Embedder.Embed(ctx, t.Model, batch)
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("embed: %d vectors for %d texts", len(vecs), len(batch))
		}
		for i, part := range batch {
			m := template
			m.Content, m.Embedding, m.Source = part, vecs[i], src
			m.Source.Part = start + i + 1
			memories = append(memories, m)
		}
	}
	return t.Store.PutMemories(ctx, memories)
}

const embedBatch = 32

// split cuts text into trimmed parts of at most max bytes, joining whole
// paragraphs while they fit and cutting longer ones at whitespace if it can.
func split(text string, max int) []string {
	var parts []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
	}
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		for len(para) > max {
			cut := strings.LastIndexAny(para[:max], " \n\t")
			if cut <= 0 {
				cut = max
				for cut > 0 && !utf8.RuneStart(para[cut]) {
					cut--
				}
				if cut == 0 {
					_, cut = utf8.DecodeRuneInString(para)
				}
			}
			flush()
			parts = append(parts, strings.TrimSpace(para[:cut]))
			para = strings.TrimSpace(para[cut:])
		}
		if para == "" {
			continue
		}
		if cur.Len() > 0 && cur.Len()+2+len(para) > max {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(para)
	}
	flush()
	return parts
}
