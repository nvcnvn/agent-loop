package store

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/nvcnvn/agent-loop/core"
)

// MemoryScope says which runs may retrieve a memory.
type MemoryScope string

// Memory scopes, widest first.
const (
	MemoryTenant   MemoryScope = "tenant"   // every run of the tenant
	MemoryProject  MemoryScope = "project"  // runs in sessions of the same project
	MemoryWorkflow MemoryScope = "workflow" // runs of the same workflow
	MemorySession  MemoryScope = "session"  // runs in the same session
)

// Memory is a piece of long-term memory, kept apart from conversation
// messages, with its embedding and where it came from.
type Memory struct {
	ID         string
	TenantID   core.TenantID
	Scope      MemoryScope
	SessionID  core.SessionID // the session a session-scoped memory belongs to
	Project    string         // the project a project-scoped memory belongs to
	Workflow   string         // the workflow a workflow-scoped memory belongs to
	Content    string
	Embedding  []float32
	EmbedModel string
	Source     MemorySource
	CreatedAt  time.Time
}

// MemorySource is a memory's provenance: the run step that saved it, or the
// document or artifact, and the part of it, that was ingested.
type MemorySource struct {
	RunID      core.RunID       `json:"run_id,omitempty"`
	SessionID  core.SessionID   `json:"session_id,omitempty"`
	TaskID     core.TaskID      `json:"task_id,omitempty"`
	StepID     core.StepID      `json:"step_id,omitempty"`
	ArtifactID string           `json:"artifact_id,omitempty"`
	Document   string           `json:"document,omitempty"`
	Part       int              `json:"part,omitempty"` // 1-based chunk of the document
	By         core.PrincipalID `json:"by,omitempty"`   // who ingested it
}

// MemoryQuery finds the tenant's memories nearest to Embedding among those
// made with EmbedModel that are tenant-scoped, or scoped to SessionID,
// Project or Workflow.
type MemoryQuery struct {
	TenantID   core.TenantID
	Embedding  []float32
	EmbedModel string
	SessionID  core.SessionID
	Project    string
	Workflow   string
	Limit      int
}

// MemoryHit is a retrieved memory and its cosine similarity to the query.
type MemoryHit struct {
	Memory
	Score float64
}

// MemoryStore persists and searches long-term memory.
type MemoryStore interface {
	PutMemory(ctx context.Context, m Memory) (string, error)
	// PutMemories saves all of ms or, on any error, none of them.
	PutMemories(ctx context.Context, ms []Memory) ([]string, error)
	SearchMemories(ctx context.Context, q MemoryQuery) ([]MemoryHit, error)
}

// ErrInvalidMemory is returned for a memory without content, embedding or a valid scope.
var ErrInvalidMemory = errors.New("invalid memory")

// ErrMemoryTooLarge is returned for a document too large to ingest.
var ErrMemoryTooLarge = errors.New("document too large to ingest")

// Validate checks the memory has what PutMemory needs.
func (m Memory) Validate() error {
	switch {
	case m.Content == "" || len(m.Embedding) == 0 || m.EmbedModel == "":
		return ErrInvalidMemory
	case m.Scope == MemorySession && m.SessionID == "", m.Scope == MemoryWorkflow && m.Workflow == "", m.Scope == MemoryProject && m.Project == "":
		return ErrInvalidMemory
	case m.Scope != MemoryTenant && m.Scope != MemorySession && m.Scope != MemoryWorkflow && m.Scope != MemoryProject:
		return ErrInvalidMemory
	}
	return nil
}

func (q MemoryQuery) visible(m Memory) bool {
	if m.TenantID != q.TenantID || m.EmbedModel != q.EmbedModel {
		return false
	}
	switch m.Scope {
	case MemorySession:
		return m.SessionID == q.SessionID
	case MemoryProject:
		return q.Project != "" && m.Project == q.Project
	case MemoryWorkflow:
		return m.Workflow == q.Workflow
	}
	return true
}

func (m *MemStore) PutMemory(ctx context.Context, mem Memory) (string, error) {
	ids, err := m.PutMemories(ctx, []Memory{mem})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

func (m *MemStore) PutMemories(_ context.Context, ms []Memory) ([]string, error) {
	ids := make([]string, len(ms))
	for i := range ms {
		if err := ms[i].Validate(); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, mem := range ms {
		if mem.ID == "" {
			mem.ID = core.NewID()
		}
		if mem.CreatedAt.IsZero() {
			mem.CreatedAt = time.Now().UTC()
		}
		m.memories = append(m.memories, mem)
		ids[i] = mem.ID
	}
	return ids, nil
}

// SearchMemories ranks by cosine similarity.
// ponytail: a linear scan; the Postgres store is the one for real volumes.
func (m *MemStore) SearchMemories(_ context.Context, q MemoryQuery) ([]MemoryHit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var hits []MemoryHit
	for _, mem := range m.memories {
		if q.visible(mem) && len(mem.Embedding) == len(q.Embedding) {
			hits = append(hits, MemoryHit{Memory: mem, Score: cosine(mem.Embedding, q.Embedding)})
		}
	}
	slices.SortStableFunc(hits, func(a, b MemoryHit) int { return cmp.Compare(b.Score, a.Score) })
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
