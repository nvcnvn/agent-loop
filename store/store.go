// Package store defines persistence ports and provides an in-memory
// implementation suitable for tests, examples and single-process deployments.
//
// A PostgreSQL implementation can be added later by satisfying the same
// interfaces; the runtime never references this package's concrete types.
package store

import (
	"context"
	"encoding/json"
	"fmt"
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
	Project   string // optional label; memories can be scoped to it
	Status    string
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Participant is a principal's membership of a session, as owner, member or viewer.
type Participant struct {
	TenantID    core.TenantID    `json:"tenant_id"`
	SessionID   core.SessionID   `json:"session_id"`
	PrincipalID core.PrincipalID `json:"principal_id"`
	Role        string           `json:"role"`
	JoinedAt    time.Time        `json:"joined_at"`
}

// AgentDefinition is an agent a tenant defined over the API, its definition
// the runtime's JSON for it; Version counts its puts.
type AgentDefinition struct {
	TenantID   core.TenantID   `json:"tenant_id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	Version    int             `json:"version"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// AgentStore keeps tenants' agent definitions.
type AgentStore interface {
	// PutAgent creates or replaces a definition and returns its new version.
	PutAgent(ctx context.Context, a AgentDefinition) (int, error)
	ListAgents(ctx context.Context, tenant core.TenantID) ([]AgentDefinition, error)
	DeleteAgent(ctx context.Context, tenant core.TenantID, name string) error
}

// Run is the stored shape of a run.
type Run struct {
	ID             core.RunID
	TenantID       core.TenantID
	SessionID      core.SessionID
	ParentRunID    core.RunID
	StartedBy      core.PrincipalID
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
	ID         core.ApprovalID  `json:"approval_id"`
	TenantID   core.TenantID    `json:"tenant_id"`
	RunID      core.RunID       `json:"run_id,omitempty"`
	StepID     core.StepID      `json:"step_id,omitempty"`
	Reason     string           `json:"reason"`
	Status     string           `json:"status"` // requested|approved|rejected|expired|canceled
	Request    json.RawMessage  `json:"request,omitempty"`
	Resolution json.RawMessage  `json:"resolution,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	ResolvedAt time.Time        `json:"resolved_at,omitzero"`
	ResolvedBy core.PrincipalID `json:"resolved_by,omitempty"`
	// ExpiresAt, when set, is the time after which only "expired" or
	// "canceled" can resolve the approval.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// ApprovalFilter selects a tenant's approvals in ID order, which is creation
// order. Empty fields select all; status "requested" leaves out approvals
// that have expired without being resolved.
type ApprovalFilter struct {
	RunID  core.RunID
	Status string
	After  core.ApprovalID // only approvals after this one
	Limit  int             // zero means no limit
	// Participant, when set, keeps the approvals of runs in sessions it participates in.
	Participant core.PrincipalID
}

// Artifact is the stored shape of an artifact: a run's input image, or an
// output a tool produced at StepID.
type Artifact struct {
	ID        string         `json:"artifact_id"`
	TenantID  core.TenantID  `json:"tenant_id"`
	SessionID core.SessionID `json:"session_id"`
	RunID     core.RunID     `json:"run_id,omitempty"`
	TaskID    core.TaskID    `json:"task_id,omitempty"`
	StepID    core.StepID    `json:"step_id,omitempty"`
	Producer  string         `json:"producer,omitempty"` // the tool that produced it
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Version   int            `json:"version"` // 1 + the latest version of the name in the session
	MimeType  string         `json:"mime_type"`
	Size      int64          `json:"size"`
	Content   []byte         `json:"-"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
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

// SessionStore manages sessions, their participants (the creator owns a session) and message projections.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) (core.SessionID, error)
	GetSession(ctx context.Context, tenant core.TenantID, id core.SessionID) (Session, error)
	// ListSessions lists the tenant's sessions, newest first, or only those
	// participant takes part in when it is set.
	ListSessions(ctx context.Context, tenant core.TenantID, participant core.PrincipalID) ([]Session, error)
	// PutParticipant adds a participant to a session of its tenant, or changes its role.
	PutParticipant(ctx context.Context, p Participant) error
	GetParticipant(ctx context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) (Participant, error)
	ListParticipants(ctx context.Context, tenant core.TenantID, session core.SessionID) ([]Participant, error)
	RemoveParticipant(ctx context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) error
	AddMessage(ctx context.Context, m MessageProjection) error
	ListMessages(ctx context.Context, tenant core.TenantID, id core.SessionID) ([]MessageProjection, error)
}

// RunStore manages run records.
type RunStore interface {
	CreateRun(ctx context.Context, r Run) (core.RunID, error)
	GetRun(ctx context.Context, tenant core.TenantID, id core.RunID) (Run, error)
	UpdateRun(ctx context.Context, r Run) error
	// UpdateRunStatus and UpdateRunUsage each write only their own columns, so
	// concurrent callers cannot undo each other. Moving to running sets
	// StartedAt if it is unset.
	UpdateRunStatus(ctx context.Context, tenant core.TenantID, id core.RunID, status core.RunStatus, reason string) error
	UpdateRunUsage(ctx context.Context, tenant core.TenantID, id core.RunID, usage core.Usage) error
	ListRuns(ctx context.Context, tenant core.TenantID, sessionID core.SessionID) ([]Run, error)
	// ListUnfinishedRuns returns every tenant's runs that are not in a terminal status; runs it cannot read are left out and reported in the error.
	ListUnfinishedRuns(ctx context.Context) ([]Run, error)
	FindByIdempotency(ctx context.Context, tenant core.TenantID, key string) (Run, error)
}

// TaskStore manages task records.
type TaskStore interface {
	CreateTask(ctx context.Context, t Task) (core.TaskID, error)
	// UpdateTask writes the task's status, output and error.
	UpdateTask(ctx context.Context, t Task) error
	ListRunTasks(ctx context.Context, tenant core.TenantID, run core.RunID) ([]Task, error)
}

// ApprovalStore manages approvals. ResolveApproval returns core.ErrConflict
// for an approval that is already resolved or expired.
type ApprovalStore interface {
	CreateApproval(ctx context.Context, a Approval) (core.ApprovalID, error)
	GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (Approval, error)
	// ResolveApproval records by, empty for the runtime itself, as who resolved it.
	ResolveApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string, by core.PrincipalID, resolution json.RawMessage) error
	WaitForApproval(ctx context.Context, id core.ApprovalID) (Approval, error)
	ListApprovals(ctx context.Context, tenant core.TenantID, f ApprovalFilter) ([]Approval, error)
	// CancelRunApprovals resolves the run's open approvals as "canceled".
	CancelRunApprovals(ctx context.Context, tenant core.TenantID, run core.RunID) error
}

// ArtifactStore stores produced artifacts.
type ArtifactStore interface {
	PutArtifact(ctx context.Context, a Artifact) (string, error)
	GetArtifact(ctx context.Context, tenant core.TenantID, id string) (Artifact, error)
	ListRunArtifacts(ctx context.Context, tenant core.TenantID, run core.RunID) ([]Artifact, error)
	// ListArtifactVersions lists the versions of a name in a session, oldest first, maybe without their content.
	ListArtifactVersions(ctx context.Context, tenant core.TenantID, session core.SessionID, name string) ([]Artifact, error)
}

// Store is the aggregate persistence interface used by runtime and api.
// Both MemStore (in this package) and the PostgreSQL driver (subpackage pg)
// satisfy it.
type Store interface {
	// Ping reports whether the backing database is reachable.
	Ping(ctx context.Context) error
	EventStore
	SessionStore
	RunStore
	TaskStore
	ApprovalStore
	ArtifactStore
	MemoryStore
	AgentStore
	RetentionStore
}

// MemStore aggregates an in-memory implementation of every store interface.
type MemStore struct {
	mu sync.RWMutex

	sessions  map[core.SessionID]Session
	members   map[core.SessionID]map[core.PrincipalID]Participant
	agents    map[core.TenantID]map[string]AgentDefinition
	runs      map[core.RunID]Run
	runByIdem map[string]core.RunID
	tasks     map[core.TaskID]Task
	messages  map[core.SessionID][]MessageProjection
	approvals map[core.ApprovalID]*approvalEntry
	artifacts map[string]Artifact
	memories  []Memory

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
		members:    map[core.SessionID]map[core.PrincipalID]Participant{},
		agents:     map[core.TenantID]map[string]AgentDefinition{},
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

// Ping always succeeds; there is no database to reach.
func (m *MemStore) Ping(context.Context) error { return nil }

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
	m.members[s.ID] = map[core.PrincipalID]Participant{}
	if s.CreatedBy != "" {
		m.members[s.ID][s.CreatedBy] = Participant{TenantID: s.TenantID, SessionID: s.ID, PrincipalID: s.CreatedBy, Role: "owner", JoinedAt: s.CreatedAt}
	}
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

func (m *MemStore) ListSessions(_ context.Context, tenant core.TenantID, participant core.PrincipalID) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Session{}
	for _, s := range m.sessions {
		if _, member := m.members[s.ID][participant]; s.TenantID == tenant && (participant == "" || member) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) PutParticipant(_ context.Context, p Participant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[p.SessionID]; !ok || s.TenantID != p.TenantID {
		return core.ErrNotFound
	}
	if p.Role != "owner" && p.Role != "member" && p.Role != "viewer" {
		return fmt.Errorf("participant role %q is not owner, member or viewer", p.Role)
	}
	p.JoinedAt = time.Now().UTC()
	if old, ok := m.members[p.SessionID][p.PrincipalID]; ok {
		p.JoinedAt = old.JoinedAt
	}
	m.members[p.SessionID][p.PrincipalID] = p
	return nil
}

func (m *MemStore) GetParticipant(_ context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) (Participant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.members[session][principal]
	if !ok || p.TenantID != tenant {
		return Participant{}, core.ErrNotFound
	}
	return p, nil
}

func (m *MemStore) ListParticipants(_ context.Context, tenant core.TenantID, session core.SessionID) ([]Participant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[session]; !ok || s.TenantID != tenant {
		return nil, core.ErrNotFound
	}
	out := make([]Participant, 0, len(m.members[session]))
	for _, p := range m.members[session] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].JoinedAt.Before(out[j].JoinedAt) || out[i].JoinedAt.Equal(out[j].JoinedAt) && out[i].PrincipalID < out[j].PrincipalID
	})
	return out, nil
}

func (m *MemStore) RemoveParticipant(_ context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.members[session][principal]; !ok || p.TenantID != tenant {
		return core.ErrNotFound
	}
	delete(m.members[session], principal)
	return nil
}

func (m *MemStore) PutAgent(_ context.Context, a AgentDefinition) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agents[a.TenantID] == nil {
		m.agents[a.TenantID] = map[string]AgentDefinition{}
	}
	a.Version = m.agents[a.TenantID][a.Name].Version + 1
	a.UpdatedAt = time.Now().UTC()
	m.agents[a.TenantID][a.Name] = a
	return a.Version, nil
}

func (m *MemStore) ListAgents(_ context.Context, tenant core.TenantID) ([]AgentDefinition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AgentDefinition, 0, len(m.agents[tenant]))
	for _, a := range m.agents[tenant] {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemStore) DeleteAgent(_ context.Context, tenant core.TenantID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[tenant][name]; !ok {
		return core.ErrNotFound
	}
	delete(m.agents[tenant], name)
	return nil
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

func (m *MemStore) UpdateRunStatus(_ context.Context, tenant core.TenantID, id core.RunID, status core.RunStatus, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.TenantID != tenant {
		return core.ErrNotFound
	}
	r.Status, r.FailureReason = status, reason
	if status == core.RunRunning && r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	m.runs[id] = r
	return nil
}

func (m *MemStore) UpdateRunUsage(_ context.Context, tenant core.TenantID, id core.RunID, usage core.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.TenantID != tenant {
		return core.ErrNotFound
	}
	r.Usage = usage
	m.runs[id] = r
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

func (m *MemStore) ListUnfinishedRuns(context.Context) ([]Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Run{}
	for _, r := range m.runs {
		if !r.Status.Terminal() {
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
	cur.Status, cur.Output, cur.Error = t.Status, t.Output, t.Error
	cur.UpdatedAt = time.Now().UTC()
	m.tasks[t.ID] = cur
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

func (m *MemStore) ResolveApproval(_ context.Context, tenant core.TenantID, id core.ApprovalID, status string, by core.PrincipalID, resolution json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.approvals[id]
	if !ok || e.approval.TenantID != tenant {
		return core.ErrNotFound
	}
	expired := !e.approval.ExpiresAt.IsZero() && !time.Now().Before(e.approval.ExpiresAt)
	if e.approval.Status != "requested" || (expired && status != "expired" && status != "canceled") {
		return core.ErrConflict
	}
	e.approval.Status = status
	e.approval.Resolution = resolution
	e.approval.ResolvedBy = by
	e.approval.ResolvedAt = time.Now().UTC()
	close(e.done)
	return nil
}

func (m *MemStore) CancelRunApprovals(_ context.Context, tenant core.TenantID, run core.RunID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.approvals {
		if e.approval.TenantID == tenant && e.approval.RunID == run && e.approval.Status == "requested" {
			e.approval.Status = "canceled"
			e.approval.ResolvedAt = time.Now().UTC()
			close(e.done)
		}
	}
	return nil
}

func (m *MemStore) ListApprovals(_ context.Context, tenant core.TenantID, f ApprovalFilter) ([]Approval, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	out := []Approval{}
	for _, e := range m.approvals {
		a := e.approval
		if a.TenantID != tenant || f.RunID != "" && a.RunID != f.RunID || f.Status != "" && a.Status != f.Status || a.ID <= f.After {
			continue
		}
		if _, member := m.members[m.runs[a.RunID].SessionID][f.Participant]; f.Participant != "" && !member {
			continue
		}
		if f.Status == "requested" && !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
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
	a.Version = 1
	for _, other := range m.artifacts {
		if other.TenantID == a.TenantID && other.SessionID == a.SessionID && other.Name == a.Name {
			a.Version = max(a.Version, other.Version+1)
		}
	}
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
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemStore) ListArtifactVersions(_ context.Context, tenant core.TenantID, session core.SessionID, name string) ([]Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Artifact{}
	for _, a := range m.artifacts {
		if a.TenantID == tenant && a.SessionID == session && a.Name == name {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ---- Events ----

// Append assigns sequence numbers atomically and fans out to subscribers.
func (m *MemStore) Append(_ context.Context, events []core.Event) ([]core.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.Event, 0, len(events))
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
		// Sending under the lock keeps delivery in sequence order and stops
		// cancel from closing a channel mid-send. A full channel drops the
		// event; clients reconnect with Last-Event-ID to recover from history.
		for _, ch := range m.subsByRun[ev.RunID] {
			select {
			case ch <- ev:
			default:
			}
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
func (m *MemStore) Subscribe(_ context.Context, run core.RunID, afterSeq int64) (<-chan core.Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextSubID
	m.nextSubID++
	var backlog []core.Event
	for _, ev := range m.events {
		if ev.RunID == run && ev.Sequence > afterSeq {
			backlog = append(backlog, ev)
		}
	}
	ch := make(chan core.Event, len(backlog)+256)
	for _, ev := range backlog {
		ch <- ev
	}
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
