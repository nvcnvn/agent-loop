package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Retention checks a store purges idle sessions with everything in them, but
// not sessions with recent activity or an unfinished run, and old memories.
func Retention(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	session := func(status core.RunStatus) (core.SessionID, core.RunID) {
		t.Helper()
		sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
		if err != nil {
			t.Fatal(err)
		}
		rid, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(ctx, []core.Event{{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvSystem, Visibility: core.VisibilityInternal}}); err != nil {
			t.Fatal(err)
		}
		return sid, rid
	}
	idle, idleRun := session(core.RunSucceeded)
	busy, busyRun := session(core.RunAwaitingApproval)
	if _, err := s.CreateTask(ctx, store.Task{TenantID: core.DefaultTenant, RunID: idleRun, AgentName: "a", Status: core.TaskCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, store.MessageProjection{SessionID: idle, Role: "user", Visibility: core.VisibilityParticipant, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	approval, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: idleRun, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: idle, RunID: idleRun, Kind: "file", Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutMemory(ctx, store.Memory{TenantID: core.DefaultTenant, Scope: store.MemorySession, SessionID: idle, Content: "kept with its session", Embedding: []float32{1}, EmbedModel: "m"}); err != nil {
		t.Fatal(err)
	}
	ingested, _ := session(core.RunSucceeded)
	empty, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(20 * time.Millisecond)
	recent, _ := session(core.RunSucceeded)
	if _, err := s.PutMemory(ctx, store.Memory{TenantID: core.DefaultTenant, Scope: store.MemorySession, SessionID: ingested, Content: "ingested just now", Embedding: []float32{1}, EmbedModel: "m"}); err != nil {
		t.Fatal(err)
	}

	if n, err := s.PurgeSessions(ctx, cutoff); err != nil || n != 2 {
		t.Fatalf("purged %d sessions, err=%v; want the idle and the empty one", n, err)
	}
	if _, err := s.GetSession(ctx, core.DefaultTenant, empty); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("empty idle session: %v", err)
	}
	if tasks, _ := s.ListRunTasks(ctx, core.DefaultTenant, idleRun); len(tasks) != 0 {
		t.Fatalf("idle session's tasks: %+v", tasks)
	}
	if _, err := s.ListMessages(ctx, core.DefaultTenant, idle); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session's messages: %v", err)
	}
	if _, err := s.GetParticipant(ctx, core.DefaultTenant, idle, core.DefaultPrincipal); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session's owner: %v", err)
	}
	if _, err := s.GetSession(ctx, core.DefaultTenant, idle); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session: %v", err)
	}
	if _, err := s.GetRun(ctx, core.DefaultTenant, idleRun); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session's run: %v", err)
	}
	if events, _ := s.ListRunEvents(ctx, core.DefaultTenant, idleRun, 0, 0); len(events) != 0 {
		t.Fatalf("idle session's events: %d", len(events))
	}
	if _, err := s.GetApproval(ctx, core.DefaultTenant, approval); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session's approval: %v", err)
	}
	if _, err := s.GetArtifact(ctx, core.DefaultTenant, artifact); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("idle session's artifact: %v", err)
	}
	if hits, _ := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: []float32{1}, EmbedModel: "m", SessionID: idle}); len(hits) != 0 {
		t.Fatalf("idle session's memories: %+v", hits)
	}
	for _, sid := range []core.SessionID{busy, recent, ingested} {
		if _, err := s.GetSession(ctx, core.DefaultTenant, sid); err != nil {
			t.Fatalf("kept session %s: %v", sid, err)
		}
	}
	if _, err := s.GetRun(ctx, core.DefaultTenant, busyRun); err != nil {
		t.Fatalf("unfinished run: %v", err)
	}

	if _, err := s.PutMemory(ctx, store.Memory{TenantID: core.DefaultTenant, Scope: store.MemoryTenant, Content: "old", Embedding: []float32{1}, EmbedModel: "m"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	before := time.Now()
	time.Sleep(20 * time.Millisecond)
	if _, err := s.PutMemory(ctx, store.Memory{TenantID: core.DefaultTenant, Scope: store.MemoryTenant, Content: "new", Embedding: []float32{1}, EmbedModel: "m"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeMemories(ctx, before); err != nil || n != 2 {
		t.Fatalf("purged %d memories, err=%v; want the old one and the one ingested earlier", n, err)
	}
	if hits, _ := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: []float32{1}, EmbedModel: "m"}); len(hits) != 1 || hits[0].Content != "new" {
		t.Fatalf("memories left: %+v", hits)
	}
}
