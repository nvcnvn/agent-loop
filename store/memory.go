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
	Workflow   string         // the workflow a workflow-scoped memory belongs to
	Content    string
	Embedding  []float32
	EmbedModel string
	Source     MemorySource
	CreatedAt  time.Time
}

// MemorySource is a memory's provenance.
type MemorySource struct {
	RunID     core.RunID     `json:"run_id,omitempty"`
	SessionID core.SessionID `json:"session_id,omitempty"`
	TaskID    core.TaskID    `json:"task_id,omitempty"`
	StepID    core.StepID    `json:"step_id,omitempty"`
}

// MemoryQuery finds the tenant's memories nearest to Embedding among those
// made with EmbedModel that are tenant-scoped, or scoped to SessionID or
// Workflow.
type MemoryQuery struct {
	TenantID   core.TenantID
	Embedding  []float32
	EmbedModel string
	SessionID  core.SessionID
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
	SearchMemories(ctx context.Context, q MemoryQuery) ([]MemoryHit, error)
}

// ErrInvalidMemory is returned for a memory without content, embedding or a valid scope.
var ErrInvalidMemory = errors.New("invalid memory")

// Validate checks the memory has what PutMemory needs.
func (m Memory) Validate() error {
	switch {
	case m.Content == "" || len(m.Embedding) == 0 || m.EmbedModel == "":
		return ErrInvalidMemory
	case m.Scope == MemorySession && m.SessionID == "", m.Scope == MemoryWorkflow && m.Workflow == "":
		return ErrInvalidMemory
	case m.Scope != MemoryTenant && m.Scope != MemorySession && m.Scope != MemoryWorkflow:
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
	case MemoryWorkflow:
		return m.Workflow == q.Workflow
	}
	return true
}

func (m *MemStore) PutMemory(_ context.Context, mem Memory) (string, error) {
	if err := mem.Validate(); err != nil {
		return "", err
	}
	if mem.ID == "" {
		mem.ID = core.NewID()
	}
	if mem.CreatedAt.IsZero() {
		mem.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memories = append(m.memories, mem)
	return mem.ID, nil
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
