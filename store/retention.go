package store

import (
	"context"
	"slices"
	"time"

	"github.com/nvcnvn/agent-loop/core"
)

// RetentionStore deletes what a retention policy no longer keeps.
type RetentionStore interface {
	// PurgeSessions deletes every session in which nothing happened since
	// idleSince (no event, no memory saved to it) and no run is unfinished, with everything in it: its runs,
	// tasks, events, messages, approvals, artifacts, participants and
	// session-scoped memories. It returns how many sessions it deleted.
	PurgeSessions(ctx context.Context, idleSince time.Time) (int, error)
	// PurgeMemories deletes the memories saved before t and returns how many.
	PurgeMemories(ctx context.Context, before time.Time) (int, error)
}

func (m *MemStore) PurgeSessions(_ context.Context, idleSince time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := map[core.SessionID]time.Time{}
	for id, s := range m.sessions {
		last[id] = s.CreatedAt
	}
	for _, ev := range m.events {
		if ev.RecordedAt.After(last[ev.SessionID]) {
			last[ev.SessionID] = ev.RecordedAt
		}
	}
	for _, mem := range m.memories {
		if mem.Scope == MemorySession && mem.CreatedAt.After(last[mem.SessionID]) {
			last[mem.SessionID] = mem.CreatedAt
		}
	}
	for _, r := range m.runs {
		if !r.Status.Terminal() {
			last[r.SessionID] = time.Now()
		}
	}
	purge := map[core.SessionID]bool{}
	for id, at := range last {
		if _, ok := m.sessions[id]; ok && at.Before(idleSince) {
			purge[id] = true
		}
	}
	runs := map[core.RunID]bool{}
	for id, r := range m.runs {
		if purge[r.SessionID] {
			runs[id] = true
			delete(m.runs, id)
			delete(m.runSeq, id)
			if r.IdempotencyKey != "" {
				delete(m.runByIdem, string(r.TenantID)+"|"+r.IdempotencyKey)
			}
		}
	}
	for id, t := range m.tasks {
		if runs[t.RunID] {
			delete(m.tasks, id)
		}
	}
	for id, a := range m.approvals {
		if runs[a.approval.RunID] {
			delete(m.approvals, id)
		}
	}
	for id, a := range m.artifacts {
		if purge[a.SessionID] {
			delete(m.artifacts, id)
		}
	}
	m.events = slices.DeleteFunc(m.events, func(ev core.Event) bool { return purge[ev.SessionID] })
	m.memories = slices.DeleteFunc(m.memories, func(mem Memory) bool { return mem.Scope == MemorySession && purge[mem.SessionID] })
	for id := range purge {
		delete(m.sessions, id)
		delete(m.members, id)
		delete(m.messages, id)
		delete(m.sessionSeq, id)
	}
	return len(purge), nil
}

func (m *MemStore) PurgeMemories(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.memories)
	m.memories = slices.DeleteFunc(m.memories, func(mem Memory) bool { return mem.CreatedAt.Before(before) })
	return n - len(m.memories), nil
}
