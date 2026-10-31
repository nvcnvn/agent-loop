// Package storetest holds conformance checks every store driver runs.
package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Memories checks scoping, ranking, limits and provenance of a MemoryStore.
// Session IDs must be UUIDs the driver accepts.
func Memories(t *testing.T, s store.MemoryStore, session, otherSession core.SessionID) {
	t.Helper()
	ctx := context.Background()
	tenant := core.DefaultTenant
	put := func(m store.Memory) {
		t.Helper()
		if m.TenantID == "" {
			m.TenantID = tenant
		}
		if m.EmbedModel == "" {
			m.EmbedModel = "m"
		}
		if _, err := s.PutMemory(ctx, m); err != nil {
			t.Fatalf("put %q: %v", m.Content, err)
		}
	}
	src := store.MemorySource{RunID: core.RunID(core.NewID()), SessionID: session, TaskID: core.TaskID(core.NewID())}
	put(store.Memory{Scope: store.MemoryTenant, Content: "closest", Embedding: []float32{1, 0, 0}, Source: src})
	put(store.Memory{Scope: store.MemorySession, SessionID: session, Content: "same session", Embedding: []float32{1, 1, 0}})
	put(store.Memory{Scope: store.MemoryWorkflow, Workflow: "support", Content: "same workflow", Embedding: []float32{0, 1, 0}})
	put(store.Memory{Scope: store.MemorySession, SessionID: otherSession, Content: "other session", Embedding: []float32{1, 0, 0}})
	put(store.Memory{Scope: store.MemoryWorkflow, Workflow: "sales", Content: "other workflow", Embedding: []float32{1, 0, 0}})
	put(store.Memory{TenantID: "00000000-0000-0000-0000-0000000000bb", Scope: store.MemoryTenant, Content: "other tenant", Embedding: []float32{1, 0, 0}})
	put(store.Memory{Scope: store.MemoryTenant, EmbedModel: "other", Content: "other model", Embedding: []float32{1, 0, 0, 0}})

	q := store.MemoryQuery{TenantID: tenant, Embedding: []float32{1, 0, 0}, EmbedModel: "m", SessionID: session, Workflow: "support"}
	hits, err := s.SearchMemories(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.Content)
	}
	if len(got) != 3 || got[0] != "closest" || got[1] != "same session" || got[2] != "same workflow" {
		t.Fatalf("hits=%q", got)
	}
	if h := hits[0]; h.Score < 0.99 || h.Source != src || h.Scope != store.MemoryTenant || h.CreatedAt.IsZero() {
		t.Fatalf("first hit=%+v", h)
	}
	if hits[1].SessionID != session || hits[2].Workflow != "support" {
		t.Fatalf("scope fields lost: %+v %+v", hits[1], hits[2])
	}

	q.Limit = 1
	if hits, _ := s.SearchMemories(ctx, q); len(hits) != 1 {
		t.Fatalf("limit 1 gave %d hits", len(hits))
	}
	if _, err := s.PutMemory(ctx, store.Memory{TenantID: tenant, Scope: store.MemorySession, Content: "x", Embedding: []float32{1}, EmbedModel: "m"}); !errors.Is(err, store.ErrInvalidMemory) {
		t.Fatalf("session memory without a session: err=%v", err)
	}
}
