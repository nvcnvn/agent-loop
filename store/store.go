// Package store defines persistence ports and provides an in-memory
// implementation suitable for tests, examples and single-process deployments.
//
// A PostgreSQL implementation can be added later by satisfying the same
// interfaces; the runtime never references this package's concrete types.
package store

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/nvcnvn/agent-loop/core"
)

// Session is the stored shape of a session.
type Session struct {
	ID        core.SessionID
	TenantID  core.TenantID
	CreatedBy core.PrincipalID
	Title     string
	Status    string
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Run is the stored shape of a run.
type Run struct {
	ID             core.RunID
	TenantID       core.TenantID
	SessionID      core.SessionID
	ParentRunID    core.RunID
	Status         core.RunStatus
	Limits         core.Limits
	Usage          core.Usage
	FailureReason  string
	IdempotencyKey string
	QueuedAt       time.Time
	StartedAt      time.Time
	EndedAt        time.Time
	Workflow       json.RawMessage
	Input          json.RawMessage
	Output         json.RawMessage
}

// Task is the stored shape of a task.
type Task struct {
	ID           core.TaskID
	TenantID     core.TenantID
	RunID        core.RunID
	ParentTaskID core.TaskID
	AgentName    string
	Status       core.TaskStatus
	Depth        int
	Attempt      int
	Input        json.RawMessage
	Output       json.RawMessage
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MessageProjection is a chat-friendly projection of an event.
type MessageProjection struct {
	ID         string
	SessionID  core.SessionID
	RunID      core.RunID
	EventID    core.EventID
	Role       string
	Visibility core.Visibility
	Content    string
	CreatedAt  time.Time
}

// Approval is the stored shape of an approval request.
type Approval struct {
	ID         core.ApprovalID
	TenantID   core.TenantID
	RunID      core.RunID
	StepID     core.StepID
	Reason     string
	Status     string // requested|approved|rejected|expired|canceled
	Request    json.RawMessage
	Resolution json.RawMessage
	CreatedAt  time.Time
	ResolvedAt time.Time
}

// Artifact is the stored shape of a produced artifact.
type Artifact struct {
	ID        string
	TenantID  core.TenantID
	SessionID core.SessionID
	RunID     core.RunID
	TaskID    core.TaskID
	Kind      string
	Name      string
	MimeType  string
	Size      int64
	Content   []byte
	Metadata  map[string]any
	CreatedAt time.Time
}

// EventStore appends events and reads them back in deterministic order.
//
// Implementations MUST allocate per-run and per-session sequence numbers
// atomically so that even concurrent appends produce a strict total order.
type EventStore interface {
	Append(ctx context.Context, events []core.Event) ([]core.Event, error)
	ListRunEvents(ctx context.Context, tenant core.TenantID, run core.RunID, afterSeq int64, limit int) ([]core.Event, error)
	ListSessionEvents(ctx context.Context, tenant core.TenantID, session core.SessionID, afterSeq int64, limit int) ([]core.Event, error)
	Subscribe(ctx context.Context, run core.RunID, afterSeq int64) (<-chan core.Event, func())
}

// SessionStore manages sessions and message projections.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) (core.SessionID, error)
	GetSession(ctx context.Context, tenant core.TenantID, id core.SessionID) (Session, error)
	ListSessions(ctx context.Context, tenant core.TenantID) ([]Session, error)
	AddMessage(ctx context.Context, m MessageProjection) error
	ListMessages(ctx context.Context, tenant core.TenantID, id core.SessionID) ([]MessageProjection, error)
}

// RunStore manages run records.
type RunStore interface {
	CreateRun(ctx context.Context, r Run) (core.RunID, error)
	GetRun(ctx context.Context, tenant core.TenantID, id core.RunID) (Run, error)
	UpdateRun(ctx context.Context, r Run) error
	ListRuns(ctx context.Context, tenant core.TenantID, sessionID core.SessionID) ([]Run, error)
	FindByIdempotency(ctx context.Context, tenant core.TenantID, key string) (Run, error)
}

// TaskStore manages task records.
type TaskStore interface {
	CreateTask(ctx context.Context, t Task) (core.TaskID, error)
	UpdateTask(ctx context.Context, t Task) error
	ListRunTasks(ctx context.Context, tenant core.TenantID, run core.RunID) ([]Task, error)
}

// ApprovalStore manages approvals.
type ApprovalStore interface {
	CreateApproval(ctx context.Context, a Approval) (core.ApprovalID, error)
	GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (Approval, error)
	ResolveApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string, resolution json.RawMessage) error
	WaitForApproval(ctx context.Context, id core.ApprovalID) (Approval, error)
}

// ArtifactStore stores produced artifacts.
type ArtifactStore interface {
	PutArtifact(ctx context.Context, a Artifact) (string, error)
	GetArtifact(ctx context.Context, tenant core.TenantID, id string) (Artifact, error)
	ListRunArtifacts(ctx context.Context, tenant core.TenantID, run core.RunID) ([]Artifact, error)
}

// Store is the aggregate persistence interface used by runtime and api.
// Both MemStore (in this package) and the PostgreSQL driver (subpackage pg)
// satisfy it.
type Store interface {
	EventStore
	SessionStore
	RunStore
	TaskStore
	ApprovalStore
	ArtifactStore
}

// MemStore aggregates an in-memory implementation of every store interface.
type MemStore struct {
	mu sync.RWMutex

	sessions  map[core.SessionID]Session
	runs      map[core.RunID]Run
	runByIdem map[string]core.RunID
	tasks     map[core.TaskID]Task
	messages  map[core.SessionID][]MessageProjection
	approvals map[core.ApprovalID]*approvalEntry
	artifacts map[string]Artifact

	events     []core.Event
	runSeq     map[core.RunID]int64
	sessionSeq map[core.SessionID]int64
	subsByRun  map[core.RunID]map[int]chan core.Event
	nextSubID  int
}

type approvalEntry struct {
	approval Approval
	done     chan struct{}
}

// NewMemStore returns a fully wired in-memory implementation.
func NewMemStore() *MemStore {
	return &MemStore{
		sessions:   map[core.SessionID]Session{},
		runs:       map[core.RunID]Run{},
		runByIdem:  map[string]core.RunID{},
		tasks:      map[core.TaskID]Task{},
		messages:   map[core.SessionID][]MessageProjection{},
		approvals:  map[core.ApprovalID]*approvalEntry{},
		artifacts:  map[string]Artifact{},
		runSeq:     map[core.RunID]int64{},
		sessionSeq: map[core.SessionID]int64{},
		subsByRun:  map[core.RunID]map[int]chan core.Event{},
	}
}

// ---- Session ----

func (m *MemStore) CreateSession(_ context.Context, s Session) (core.SessionID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == "" {
		s.ID = core.SessionID(core.NewID())
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.UpdatedAt = s.CreatedAt
	if s.Status == "" {
		s.Status = "active"
	}
	m.sessions[s.ID] = s
	return s.ID, nil
}

func (m *MemStore) GetSession(_ context.Context, tenant core.TenantID, id core.SessionID) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok || s.TenantID != tenant {
		return Session{}, core.ErrNotFound
	}
	return s, nil
}

func (m *MemStore) ListSessions(_ context.Context, tenant core.TenantID) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Session{}
	for _, s := range m.sessions {
		if s.TenantID == tenant {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) AddMessage(_ context.Context, msg MessageProjection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.ID == "" {
		msg.ID = core.NewID()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	m.messages[msg.SessionID] = append(m.messages[msg.SessionID], msg)
	return nil
}

func (m *MemStore) ListMessages(_ context.Context, tenant core.TenantID, id core.SessionID) ([]MessageProjection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok || s.TenantID != tenant {
		return nil, core.ErrNotFound
	}
	out := make([]MessageProjection, len(m.messages[id]))
	copy(out, m.messages[id])
	return out, nil
}

// ---- Run ----

func (m *MemStore) CreateRun(_ context.Context, r Run) (core.RunID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.IdempotencyKey != "" {
		if existing, ok := m.runByIdem[string(r.TenantID)+"|"+r.IdempotencyKey]; ok {
			return existing, core.ErrConflict
		}
	}
	if r.ID == "" {
		r.ID = core.RunID(core.NewID())
	}
	if r.QueuedAt.IsZero() {
		r.QueuedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = core.RunQueued
	}
	m.runs[r.ID] = r
	if r.IdempotencyKey != "" {
		m.runByIdem[string(r.TenantID)+"|"+r.IdempotencyKey] = r.ID
	}
	return r.ID, nil
}

func (m *MemStore) GetRun(_ context.Context, tenant core.TenantID, id core.RunID) (Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok || r.TenantID != tenant {
		return Run{}, core.ErrNotFound
	}
	return r, nil
}

func (m *MemStore) UpdateRun(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.runs[r.ID]
	if !ok || cur.TenantID != r.TenantID {
		return core.ErrNotFound
	}
	m.runs[r.ID] = r
	return nil
}

func (m *MemStore) ListRuns(_ context.Context, tenant core.TenantID, sessionID core.SessionID) ([]Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Run{}
	for _, r := range m.runs {
		if r.TenantID == tenant && r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.Before(out[j].QueuedAt) })
	return out, nil
}

func (m *MemStore) FindByIdempotency(_ context.Context, tenant core.TenantID, key string) (Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.runByIdem[string(tenant)+"|"+key]
	if !ok {
		return Run{}, core.ErrNotFound
	}
	return m.runs[id], nil
}

// ---- Task ----

func (m *MemStore) CreateTask(_ context.Context, t Task) (core.TaskID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		t.ID = core.TaskID(core.NewID())
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.UpdatedAt = t.CreatedAt
	if t.Status == "" {
		t.Status = core.TaskPending
	}
	if t.Attempt == 0 {
		t.Attempt = 1
	}
	m.tasks[t.ID] = t
	return t.ID, nil
}

func (m *MemStore) UpdateTask(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.tasks[t.ID]
	if !ok || cur.TenantID != t.TenantID {
		return core.ErrNotFound
	}
	t.UpdatedAt = time.Now().UTC()
	m.tasks[t.ID] = t
	return nil
}

func (m *MemStore) ListRunTasks(_ context.Context, tenant core.TenantID, run core.RunID) ([]Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Task{}
	for _, t := range m.tasks {
		if t.TenantID == tenant && t.RunID == run {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ---- Approval ----

func (m *MemStore) CreateApproval(_ context.Context, a Approval) (core.ApprovalID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == "" {
		a.ID = core.ApprovalID(core.NewID())
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.Status == "" {
		a.Status = "requested"
	}
	m.approvals[a.ID] = &approvalEntry{approval: a, done: make(chan struct{})}
	return a.ID, nil
}

func (m *MemStore) GetApproval(_ context.Context, tenant core.TenantID, id core.ApprovalID) (Approval, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.approvals[id]
	if !ok || e.approval.TenantID != tenant {
		return Approval{}, core.ErrNotFound
	}
	return e.approval, nil
}

func (m *MemStore) ResolveApproval(_ context.Context, tenant core.TenantID, id core.ApprovalID, status string, resolution json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.approvals[id]
	if !ok || e.approval.TenantID != tenant {
		return core.ErrNotFound
	}
	if e.approval.Status != "requested" {
		return core.ErrConflict
	}
	e.approval.Status = status
	e.approval.Resolution = resolution
	e.approval.ResolvedAt = time.Now().UTC()
	close(e.done)
	return nil
}

func (m *MemStore) WaitForApproval(ctx context.Context, id core.ApprovalID) (Approval, error) {
	m.mu.RLock()
	e, ok := m.approvals[id]
	m.mu.RUnlock()
	if !ok {
		return Approval{}, core.ErrNotFound
	}
	select {
	case <-ctx.Done():
		return Approval{}, ctx.Err()
	case <-e.done:
		m.mu.RLock()
		defer m.mu.RUnlock()
		return e.approval, nil
	}
}

// ---- Artifact ----

func (m *MemStore) PutArtifact(_ context.Context, a Artifact) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == "" {
		a.ID = core.NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.MimeType == "" {
		a.MimeType = "application/octet-stream"
	}
	a.Size = int64(len(a.Content))
	m.artifacts[a.ID] = a
	return a.ID, nil
}

func (m *MemStore) GetArtifact(_ context.Context, tenant core.TenantID, id string) (Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.artifacts[id]
	if !ok || a.TenantID != tenant {
		return Artifact{}, core.ErrNotFound
	}
	return a, nil
}

func (m *MemStore) ListRunArtifacts(_ context.Context, tenant core.TenantID, run core.RunID) ([]Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Artifact{}
	for _, a := range m.artifacts {
		if a.TenantID == tenant && a.RunID == run {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ---- Events ----

// Append assigns sequence numbers atomically and fans out to subscribers.
func (m *MemStore) Append(_ context.Context, events []core.Event) ([]core.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	out := make([]core.Event, 0, len(events))
	subsToNotify := []struct {
		ch chan core.Event
		ev core.Event
	}{}
	for _, ev := range events {
		if ev.ID == "" {
			ev.ID = core.EventID(core.NewID())
		}
		if ev.OccurredAt.IsZero() {
			ev.OccurredAt = time.Now().UTC()
		}
		ev.RecordedAt = time.Now().UTC()
		if ev.RunID != "" {
			m.runSeq[ev.RunID]++
			ev.Sequence = m.runSeq[ev.RunID]
		}
		if ev.SessionID != "" {
			m.sessionSeq[ev.SessionID]++
			ev.SessionSequence = m.sessionSeq[ev.SessionID]
		}
		if ev.CorrelationID == "" {
			ev.CorrelationID = core.NewID()
		}
		m.events = append(m.events, ev)
		out = append(out, ev)
		if ev.RunID != "" {
			for _, ch := range m.subsByRun[ev.RunID] {
				subsToNotify = append(subsToNotify, struct {
					ch chan core.Event
					ev core.Event
				}{ch, ev})
			}
		}
	}
	m.mu.Unlock()

	for _, n := range subsToNotify {
		select {
		case n.ch <- n.ev:
		default:
			// Slow subscriber drops events; clients should reconnect with
			// Last-Event-ID to recover from history.
		}
	}
	return out, nil
}

func (m *MemStore) ListRunEvents(_ context.Context, tenant core.TenantID, run core.RunID, afterSeq int64, limit int) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []core.Event{}
	for _, ev := range m.events {
		if ev.RunID == run && ev.TenantID == tenant && ev.Sequence > afterSeq {
			out = append(out, ev)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *MemStore) ListSessionEvents(_ context.Context, tenant core.TenantID, session core.SessionID, afterSeq int64, limit int) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []core.Event{}
	for _, ev := range m.events {
		if ev.SessionID == session && ev.TenantID == tenant && ev.SessionSequence > afterSeq {
			out = append(out, ev)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// Subscribe streams new events for a run. The returned cancel func detaches
// the subscription. The channel is buffered; on overflow events are dropped
// and the client must catch up with ListRunEvents using Last-Event-ID.
func (m *MemStore) Subscribe(_ context.Context, run core.RunID, _ int64) (<-chan core.Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextSubID
	m.nextSubID++
	ch := make(chan core.Event, 256)
	if m.subsByRun[run] == nil {
		m.subsByRun[run] = map[int]chan core.Event{}
	}
	m.subsByRun[run][id] = ch
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if subs, ok := m.subsByRun[run]; ok {
			if c, ok := subs[id]; ok {
				delete(subs, id)
				close(c)
			}
		}
	}
	return ch, cancel
}
