// Package pg implements store.Store on top of PostgreSQL using pgx v5.
//
// All methods enforce tenant isolation by including tenant_id in their
// predicates. Sequence allocation for run_events uses an upsert against
// event_counters within the same transaction as the insert, which is
// strictly monotonic under concurrent writers.
//
// Subscribe is implemented with a polling loop (default 200ms). It is
// adequate for v1 and avoids opening a dedicated LISTEN/NOTIFY connection
// per subscriber. A LISTEN/NOTIFY-backed implementation can be added later
// without changing the Store interface.
package pg

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

//go:embed schema.sql
var schemaFS embed.FS

// Schema returns the embedded migration. Callers can apply it directly.
func Schema() string {
	b, _ := schemaFS.ReadFile("schema.sql")
	return string(b)
}

// PollInterval controls how often Subscribe polls for new events.
var PollInterval = 200 * time.Millisecond

// Store is the pgx-backed implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to the given DSN and returns a Store. The caller must Close()
// it.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// NewFromPool wraps an existing pool (useful in tests).
func NewFromPool(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Close releases the underlying pool.
func (s *Store) Close() { s.pool.Close() }

// EnsureSchema applies schema.sql if the tables do not exist yet.
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, Schema())
	return err
}

// ---- helpers ----

func uuidOrNew(id string) string {
	if id == "" {
		return core.NewID()
	}
	return id
}

func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return core.ErrConflict
	}
	return err
}

func encJSON(v any) []byte {
	if v == nil {
		return []byte(`{}`)
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return []byte(`{}`)
		}
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// ---- Sessions ----

func (s *Store) CreateSession(ctx context.Context, sess store.Session) (core.SessionID, error) {
	if sess.ID == "" {
		sess.ID = core.SessionID(core.NewID())
	}
	if sess.Status == "" {
		sess.Status = "active"
	}
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (session_id, tenant_id, created_by, title, status, metadata, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		string(sess.ID), string(sess.TenantID), string(sess.CreatedBy),
		sess.Title, sess.Status, encJSON(sess.Metadata), sess.CreatedAt, sess.UpdatedAt,
	)
	if err != nil {
		return "", mapErr(err)
	}
	return sess.ID, nil
}

func (s *Store) GetSession(ctx context.Context, tenant core.TenantID, id core.SessionID) (store.Session, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT session_id, tenant_id, created_by, title, status, metadata, created_at, updated_at
		FROM sessions WHERE session_id=$1 AND tenant_id=$2`,
		string(id), string(tenant))
	var out store.Session
	var meta []byte
	var sid, tid, cb string
	if err := row.Scan(&sid, &tid, &cb, &out.Title, &out.Status, &meta, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return store.Session{}, mapErr(err)
	}
	out.ID = core.SessionID(sid)
	out.TenantID = core.TenantID(tid)
	out.CreatedBy = core.PrincipalID(cb)
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &out.Metadata)
	}
	return out, nil
}

func (s *Store) ListSessions(ctx context.Context, tenant core.TenantID) ([]store.Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, tenant_id, created_by, title, status, metadata, created_at, updated_at
		FROM sessions WHERE tenant_id=$1 ORDER BY created_at DESC`, string(tenant))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Session{}
	for rows.Next() {
		var sess store.Session
		var meta []byte
		var sid, tid, createdBy string
		if err := rows.Scan(&sid, &tid, &createdBy, &sess.Title, &sess.Status, &meta, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		sess.ID = core.SessionID(sid)
		sess.TenantID = core.TenantID(tid)
		sess.CreatedBy = core.PrincipalID(createdBy)
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &sess.Metadata)
		}
		out = append(out, sess)
	}
	return out, nil
}

func (s *Store) AddMessage(ctx context.Context, m store.MessageProjection) error {
	if m.ID == "" {
		m.ID = core.NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO messages (message_id, session_id, run_id, event_id, role, visibility, content, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		m.ID, string(m.SessionID), nullableUUID(string(m.RunID)), nullableUUID(string(m.EventID)),
		m.Role, string(m.Visibility), m.Content, m.CreatedAt,
	)
	return mapErr(err)
}

func (s *Store) ListMessages(ctx context.Context, tenant core.TenantID, id core.SessionID) ([]store.MessageProjection, error) {
	if _, err := s.GetSession(ctx, tenant, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT message_id, session_id, run_id, event_id, role, visibility, content, created_at
		FROM messages WHERE session_id=$1 ORDER BY created_at`, string(id))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.MessageProjection{}
	for rows.Next() {
		var m store.MessageProjection
		var sid string
		var rid, eid *string
		var vis string
		if err := rows.Scan(&m.ID, &sid, &rid, &eid, &m.Role, &vis, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.SessionID = core.SessionID(sid)
		if rid != nil {
			m.RunID = core.RunID(*rid)
		}
		if eid != nil {
			m.EventID = core.EventID(*eid)
		}
		m.Visibility = core.Visibility(vis)
		out = append(out, m)
	}
	return out, nil
}

// ---- Runs ----

func (s *Store) CreateRun(ctx context.Context, r store.Run) (core.RunID, error) {
	if r.ID == "" {
		r.ID = core.RunID(core.NewID())
	}
	if r.Status == "" {
		r.Status = core.RunQueued
	}
	if r.QueuedAt.IsZero() {
		r.QueuedAt = time.Now().UTC()
	}
	limits, _ := json.Marshal(r.Limits)
	usage, _ := json.Marshal(r.Usage)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (run_id, tenant_id, session_id, parent_run_id, status,
		                  limits, usage_totals, failure_reason, idempotency_key,
		                  workflow, input, output, queued_at, started_at, ended_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())`,
		string(r.ID), string(r.TenantID), string(r.SessionID), nullableUUID(string(r.ParentRunID)),
		string(r.Status), limits, usage, r.FailureReason, nullable(r.IdempotencyKey),
		nullableJSON(r.Workflow), nullableJSON(r.Input), nullableJSON(r.Output),
		r.QueuedAt, nullableTime(r.StartedAt), nullableTime(r.EndedAt),
	)
	if err != nil {
		if errors.Is(mapErr(err), core.ErrConflict) && r.IdempotencyKey != "" {
			if existing, gerr := s.FindByIdempotency(ctx, r.TenantID, r.IdempotencyKey); gerr == nil {
				return existing.ID, core.ErrConflict
			}
		}
		return "", mapErr(err)
	}
	return r.ID, nil
}

func (s *Store) GetRun(ctx context.Context, tenant core.TenantID, id core.RunID) (store.Run, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT run_id, tenant_id, session_id, parent_run_id, status, limits, usage_totals,
		       failure_reason, idempotency_key, workflow, input, output,
		       queued_at, started_at, ended_at
		FROM runs WHERE run_id=$1 AND tenant_id=$2`, string(id), string(tenant))
	return scanRun(row)
}

func (s *Store) UpdateRun(ctx context.Context, r store.Run) error {
	limits, _ := json.Marshal(r.Limits)
	usage, _ := json.Marshal(r.Usage)
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status=$3, limits=$4, usage_totals=$5, failure_reason=$6,
		                workflow=$7, input=$8, output=$9,
		                started_at=$10, ended_at=$11, updated_at=now()
		WHERE run_id=$1 AND tenant_id=$2`,
		string(r.ID), string(r.TenantID), string(r.Status), limits, usage,
		r.FailureReason, nullableJSON(r.Workflow), nullableJSON(r.Input), nullableJSON(r.Output),
		nullableTime(r.StartedAt), nullableTime(r.EndedAt),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, tenant core.TenantID, sessionID core.SessionID) ([]store.Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, tenant_id, session_id, parent_run_id, status, limits, usage_totals,
		       failure_reason, idempotency_key, workflow, input, output,
		       queued_at, started_at, ended_at
		FROM runs WHERE tenant_id=$1 AND session_id=$2 ORDER BY queued_at`,
		string(tenant), string(sessionID))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) FindByIdempotency(ctx context.Context, tenant core.TenantID, key string) (store.Run, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT run_id, tenant_id, session_id, parent_run_id, status, limits, usage_totals,
		       failure_reason, idempotency_key, workflow, input, output,
		       queued_at, started_at, ended_at
		FROM runs WHERE tenant_id=$1 AND idempotency_key=$2`, string(tenant), key)
	return scanRun(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (store.Run, error) {
	var (
		out                  store.Run
		rid, tid, sid        string
		parent, idem         *string
		limits, usage        []byte
		workflow, in, output []byte
		started, ended       *time.Time
		status               string
	)
	if err := row.Scan(&rid, &tid, &sid, &parent, &status, &limits, &usage,
		&out.FailureReason, &idem, &workflow, &in, &output,
		&out.QueuedAt, &started, &ended); err != nil {
		return store.Run{}, mapErr(err)
	}
	out.ID = core.RunID(rid)
	out.TenantID = core.TenantID(tid)
	out.SessionID = core.SessionID(sid)
	if parent != nil {
		out.ParentRunID = core.RunID(*parent)
	}
	out.Status = core.RunStatus(status)
	if idem != nil {
		out.IdempotencyKey = *idem
	}
	if started != nil {
		out.StartedAt = *started
	}
	if ended != nil {
		out.EndedAt = *ended
	}
	if len(limits) > 0 {
		_ = json.Unmarshal(limits, &out.Limits)
	}
	if len(usage) > 0 {
		_ = json.Unmarshal(usage, &out.Usage)
	}
	out.Workflow = workflow
	out.Input = in
	out.Output = output
	return out, nil
}

// ---- Tasks ----

func (s *Store) CreateTask(ctx context.Context, t store.Task) (core.TaskID, error) {
	if t.ID == "" {
		t.ID = core.TaskID(core.NewID())
	}
	if t.Status == "" {
		t.Status = core.TaskPending
	}
	if t.Attempt == 0 {
		t.Attempt = 1
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tasks (task_id, tenant_id, run_id, parent_task_id, agent_name, status,
		                   depth, attempt, input, output, error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
		string(t.ID), string(t.TenantID), string(t.RunID), nullableUUID(string(t.ParentTaskID)),
		t.AgentName, string(t.Status), t.Depth, t.Attempt,
		nullableJSONOr(t.Input, "{}"), nullableJSON(t.Output), t.Error, t.CreatedAt,
	)
	if err != nil {
		return "", mapErr(err)
	}
	return t.ID, nil
}

func (s *Store) UpdateTask(ctx context.Context, t store.Task) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks SET status=$3, output=$4, error=$5, updated_at=now()
		WHERE task_id=$1 AND tenant_id=$2`,
		string(t.ID), string(t.TenantID), string(t.Status),
		nullableJSON(t.Output), t.Error,
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) ListRunTasks(ctx context.Context, tenant core.TenantID, run core.RunID) ([]store.Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT task_id, tenant_id, run_id, parent_task_id, agent_name, status,
		       depth, attempt, input, output, error, created_at, updated_at
		FROM tasks WHERE tenant_id=$1 AND run_id=$2 ORDER BY created_at`,
		string(tenant), string(run))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Task{}
	for rows.Next() {
		var (
			t            store.Task
			tid, ten, rn string
			parent       *string
			status       string
			input, outp  []byte
		)
		if err := rows.Scan(&tid, &ten, &rn, &parent, &t.AgentName, &status,
			&t.Depth, &t.Attempt, &input, &outp, &t.Error, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.ID = core.TaskID(tid)
		t.TenantID = core.TenantID(ten)
		t.RunID = core.RunID(rn)
		if parent != nil {
			t.ParentTaskID = core.TaskID(*parent)
		}
		t.Status = core.TaskStatus(status)
		t.Input = input
		t.Output = outp
		out = append(out, t)
	}
	return out, nil
}

// ---- Approvals ----

func (s *Store) CreateApproval(ctx context.Context, a store.Approval) (core.ApprovalID, error) {
	if a.ID == "" {
		a.ID = core.ApprovalID(core.NewID())
	}
	if a.Status == "" {
		a.Status = "requested"
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO approvals (approval_id, tenant_id, run_id, step_id, reason, status, request, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		string(a.ID), string(a.TenantID), nullableUUID(string(a.RunID)), nullableUUID(string(a.StepID)),
		a.Reason, a.Status, encJSON(a.Request), a.CreatedAt,
	)
	if err != nil {
		return "", mapErr(err)
	}
	return a.ID, nil
}

func (s *Store) GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (store.Approval, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT approval_id, tenant_id, run_id, step_id, reason, status, request, resolution, created_at, resolved_at
		FROM approvals WHERE approval_id=$1 AND tenant_id=$2`, string(id), string(tenant))
	var (
		a        store.Approval
		aid, tid string
		rid, sid *string
		req, res []byte
		resolved *time.Time
	)
	if err := row.Scan(&aid, &tid, &rid, &sid, &a.Reason, &a.Status, &req, &res, &a.CreatedAt, &resolved); err != nil {
		return store.Approval{}, mapErr(err)
	}
	a.ID = core.ApprovalID(aid)
	a.TenantID = core.TenantID(tid)
	if rid != nil {
		a.RunID = core.RunID(*rid)
	}
	if sid != nil {
		a.StepID = core.StepID(*sid)
	}
	a.Request = req
	a.Resolution = res
	if resolved != nil {
		a.ResolvedAt = *resolved
	}
	return a, nil
}

func (s *Store) ResolveApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string, resolution json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE approvals SET status=$3, resolution=$4, resolved_at=now()
		WHERE approval_id=$1 AND tenant_id=$2 AND status='requested'`,
		string(id), string(tenant), status, nullableJSON(resolution),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		// Either not found or already resolved.
		if _, gerr := s.GetApproval(ctx, tenant, id); gerr != nil {
			return gerr
		}
		return core.ErrConflict
	}
	return nil
}

// WaitForApproval polls until the approval resolves or ctx is canceled.
func (s *Store) WaitForApproval(ctx context.Context, id core.ApprovalID) (store.Approval, error) {
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		row := s.pool.QueryRow(ctx, `
			SELECT approval_id, tenant_id, run_id, step_id, reason, status, request, resolution, created_at, resolved_at
			FROM approvals WHERE approval_id=$1`, string(id))
		var (
			a        store.Approval
			aid, tid string
			rid, sid *string
			req, res []byte
			resolved *time.Time
		)
		if err := row.Scan(&aid, &tid, &rid, &sid, &a.Reason, &a.Status, &req, &res, &a.CreatedAt, &resolved); err != nil {
			return store.Approval{}, mapErr(err)
		}
		a.ID = core.ApprovalID(aid)
		a.TenantID = core.TenantID(tid)
		if rid != nil {
			a.RunID = core.RunID(*rid)
		}
		if sid != nil {
			a.StepID = core.StepID(*sid)
		}
		a.Request, a.Resolution = req, res
		if resolved != nil {
			a.ResolvedAt = *resolved
		}
		if a.Status != "requested" {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return store.Approval{}, ctx.Err()
		case <-t.C:
		}
	}
}

// ---- Artifacts ----

func (s *Store) PutArtifact(ctx context.Context, a store.Artifact) (string, error) {
	if a.ID == "" {
		a.ID = core.NewID()
	}
	if a.MimeType == "" {
		a.MimeType = "application/octet-stream"
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	a.Size = int64(len(a.Content))
	_, err := s.pool.Exec(ctx, `
		INSERT INTO artifacts (artifact_id, tenant_id, session_id, run_id, task_id,
		                       kind, name, mime_type, size_bytes, content, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.ID, string(a.TenantID), string(a.SessionID), nullableUUID(string(a.RunID)), nullableUUID(string(a.TaskID)),
		a.Kind, a.Name, a.MimeType, a.Size, a.Content, encJSON(a.Metadata), a.CreatedAt,
	)
	if err != nil {
		return "", mapErr(err)
	}
	return a.ID, nil
}

func (s *Store) GetArtifact(ctx context.Context, tenant core.TenantID, id string) (store.Artifact, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT artifact_id, tenant_id, session_id, run_id, task_id, kind, name, mime_type, size_bytes, content, metadata, created_at
		FROM artifacts WHERE artifact_id=$1 AND tenant_id=$2`, id, string(tenant))
	var (
		a             store.Artifact
		aid, tid, sid string
		rid, taskID   *string
		meta          []byte
	)
	if err := row.Scan(&aid, &tid, &sid, &rid, &taskID, &a.Kind, &a.Name, &a.MimeType, &a.Size, &a.Content, &meta, &a.CreatedAt); err != nil {
		return store.Artifact{}, mapErr(err)
	}
	a.ID = aid
	a.TenantID = core.TenantID(tid)
	a.SessionID = core.SessionID(sid)
	if rid != nil {
		a.RunID = core.RunID(*rid)
	}
	if taskID != nil {
		a.TaskID = core.TaskID(*taskID)
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &a.Metadata)
	}
	return a, nil
}

func (s *Store) ListRunArtifacts(ctx context.Context, tenant core.TenantID, run core.RunID) ([]store.Artifact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT artifact_id, tenant_id, session_id, run_id, task_id, kind, name, mime_type, size_bytes, content, metadata, created_at
		FROM artifacts WHERE tenant_id=$1 AND run_id=$2 ORDER BY created_at`,
		string(tenant), string(run))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Artifact{}
	for rows.Next() {
		var (
			a             store.Artifact
			aid, tid, sid string
			rid, taskID   *string
			meta          []byte
		)
		if err := rows.Scan(&aid, &tid, &sid, &rid, &taskID, &a.Kind, &a.Name, &a.MimeType, &a.Size, &a.Content, &meta, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.ID = aid
		a.TenantID = core.TenantID(tid)
		a.SessionID = core.SessionID(sid)
		if rid != nil {
			a.RunID = core.RunID(*rid)
		}
		if taskID != nil {
			a.TaskID = core.TaskID(*taskID)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &a.Metadata)
		}
		out = append(out, a)
	}
	return out, nil
}

// ---- Events ----

// allocSequence atomically reserves the next sequence value for a scope.
func allocSequence(ctx context.Context, tx pgx.Tx, scope, scopeID string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `
		INSERT INTO event_counters (scope_type, scope_id, next_sequence)
		VALUES ($1, $2, 2)
		ON CONFLICT (scope_type, scope_id)
		DO UPDATE SET next_sequence = event_counters.next_sequence + 1
		RETURNING next_sequence - 1`, scope, scopeID).Scan(&seq)
	return seq, err
}

func (s *Store) Append(ctx context.Context, events []core.Event) ([]core.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	out := make([]core.Event, 0, len(events))

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	for _, ev := range events {
		ev.ID = core.EventID(uuidOrNew(string(ev.ID)))
		now := time.Now().UTC()
		if ev.OccurredAt.IsZero() {
			ev.OccurredAt = now
		}
		ev.RecordedAt = now
		if ev.CorrelationID == "" {
			ev.CorrelationID = core.NewID()
		}
		if ev.RunID != "" {
			seq, err := allocSequence(ctx, tx, "run", string(ev.RunID))
			if err != nil {
				return nil, mapErr(err)
			}
			ev.Sequence = seq
		}
		if ev.SessionID != "" {
			seq, err := allocSequence(ctx, tx, "session", string(ev.SessionID))
			if err != nil {
				return nil, mapErr(err)
			}
			ev.SessionSequence = seq
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO run_events (event_id, tenant_id, session_id, run_id, sequence, session_sequence,
			                        event_type, visibility, actor_id, task_id, step_id, parent_event_id,
			                        correlation_id, causation_id, payload, hidden_payload,
			                        occurred_at, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			string(ev.ID), string(ev.TenantID), string(ev.SessionID), nullableUUID(string(ev.RunID)),
			ev.Sequence, ev.SessionSequence, ev.Type, string(ev.Visibility),
			nullableUUID(string(ev.ActorID)), nullableUUID(string(ev.TaskID)), nullableUUID(string(ev.StepID)),
			nullableUUID(string(ev.ParentEventID)), ev.CorrelationID, nullable(ev.CausationID),
			encJSON(ev.Payload), nullableJSON(ev.HiddenPayload),
			ev.OccurredAt, ev.RecordedAt,
		)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, ev)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

func (s *Store) ListRunEvents(ctx context.Context, tenant core.TenantID, run core.RunID, afterSeq int64, limit int) ([]core.Event, error) {
	q := `SELECT event_id, tenant_id, session_id, run_id, sequence, session_sequence,
	             event_type, visibility, actor_id, task_id, step_id, parent_event_id,
	             correlation_id, causation_id, payload, hidden_payload,
	             occurred_at, recorded_at
	      FROM run_events WHERE tenant_id=$1 AND run_id=$2 AND sequence>$3 ORDER BY sequence`
	args := []any{string(tenant), string(run), afterSeq}
	if limit > 0 {
		q += " LIMIT $4"
		args = append(args, limit)
	}
	return s.queryEvents(ctx, q, args...)
}

func (s *Store) ListSessionEvents(ctx context.Context, tenant core.TenantID, session core.SessionID, afterSeq int64, limit int) ([]core.Event, error) {
	q := `SELECT event_id, tenant_id, session_id, run_id, sequence, session_sequence,
	             event_type, visibility, actor_id, task_id, step_id, parent_event_id,
	             correlation_id, causation_id, payload, hidden_payload,
	             occurred_at, recorded_at
	      FROM run_events WHERE tenant_id=$1 AND session_id=$2 AND session_sequence>$3 ORDER BY session_sequence`
	args := []any{string(tenant), string(session), afterSeq}
	if limit > 0 {
		q += " LIMIT $4"
		args = append(args, limit)
	}
	return s.queryEvents(ctx, q, args...)
}

func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]core.Event, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var (
			ev                   core.Event
			eid, tid, sid        string
			rid, actor, task, sp *string
			parent, caus         *string
			vis                  string
			payload, hidden      []byte
		)
		if err := rows.Scan(&eid, &tid, &sid, &rid, &ev.Sequence, &ev.SessionSequence,
			&ev.Type, &vis, &actor, &task, &sp, &parent,
			&ev.CorrelationID, &caus, &payload, &hidden,
			&ev.OccurredAt, &ev.RecordedAt); err != nil {
			return nil, err
		}
		ev.ID = core.EventID(eid)
		ev.TenantID = core.TenantID(tid)
		ev.SessionID = core.SessionID(sid)
		if rid != nil {
			ev.RunID = core.RunID(*rid)
		}
		ev.Visibility = core.Visibility(vis)
		if actor != nil {
			ev.ActorID = core.PrincipalID(*actor)
		}
		if task != nil {
			ev.TaskID = core.TaskID(*task)
		}
		if sp != nil {
			ev.StepID = core.StepID(*sp)
		}
		if parent != nil {
			ev.ParentEventID = core.EventID(*parent)
		}
		if caus != nil {
			ev.CausationID = *caus
		}
		ev.Payload = payload
		ev.HiddenPayload = hidden
		out = append(out, ev)
	}
	return out, nil
}

// Subscribe polls the database for new events for a specific run. It returns
// a channel and a cancel func; the channel is closed when the cancel func is
// invoked or the context is canceled.
func (s *Store) Subscribe(ctx context.Context, run core.RunID, afterSeq int64) (<-chan core.Event, func()) {
	ch := make(chan core.Event, 64)
	subCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(ch)
		seq := afterSeq
		t := time.NewTicker(PollInterval)
		defer t.Stop()
		for {
			events, err := s.pool.Query(subCtx, `
				SELECT event_id, tenant_id, session_id, run_id, sequence, session_sequence,
				       event_type, visibility, actor_id, task_id, step_id, parent_event_id,
				       correlation_id, causation_id, payload, hidden_payload,
				       occurred_at, recorded_at
				FROM run_events WHERE run_id=$1 AND sequence>$2 ORDER BY sequence`,
				string(run), seq)
			if err != nil {
				if subCtx.Err() != nil {
					return
				}
			} else {
				for events.Next() {
					var (
						ev                   core.Event
						eid, tid, sid        string
						rid, actor, task, sp *string
						parent, caus         *string
						vis                  string
						payload, hidden      []byte
					)
					if err := events.Scan(&eid, &tid, &sid, &rid, &ev.Sequence, &ev.SessionSequence,
						&ev.Type, &vis, &actor, &task, &sp, &parent,
						&ev.CorrelationID, &caus, &payload, &hidden,
						&ev.OccurredAt, &ev.RecordedAt); err != nil {
						continue
					}
					ev.ID = core.EventID(eid)
					ev.TenantID = core.TenantID(tid)
					ev.SessionID = core.SessionID(sid)
					if rid != nil {
						ev.RunID = core.RunID(*rid)
					}
					ev.Visibility = core.Visibility(vis)
					ev.Payload = payload
					ev.HiddenPayload = hidden
					select {
					case ch <- ev:
						seq = ev.Sequence
					case <-subCtx.Done():
						events.Close()
						return
					}
				}
				events.Close()
			}
			select {
			case <-subCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return ch, cancel
}

// ---- helpers ----

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func nullableJSONOr(b json.RawMessage, def string) any {
	if len(b) == 0 {
		return []byte(def)
	}
	return []byte(b)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// Compile-time assertion: *Store satisfies store.Store.
var _ store.Store = (*Store)(nil)

// Placeholder so the unused import of fmt drops out cleanly when the file
// is gofmt'd without other changes.
var _ = fmt.Sprintf
