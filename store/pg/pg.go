// Package pg implements store.Store on top of PostgreSQL using pgx v5.
//
// All methods enforce tenant isolation by including tenant_id in their
// predicates. Sequence allocation for run_events uses an upsert against
// event_counters within the same transaction as the insert, which is
// strictly monotonic under concurrent writers.
//
// Subscribe and WaitForApproval are pushed: Append and resolving approvals
// NOTIFY, and each Store LISTENs on one connection of its own, outside the
// pool, so the database must allow session-level LISTEN (not a
// transaction-mode pooler). They also look again every Store.Recheck.
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Store is the pgx-backed implementation of store.Store.
type Store struct {
	// Sealer, when set, encrypts the sensitive columns it names before they are written.
	Sealer *Sealer
	pool   *pgxpool.Pool
	notify *notifier
	// Recheck is how often Subscribe and WaitForApproval look again without a notification.
	Recheck time.Duration
}

// New connects to the given DSN and returns a Store. The caller must Close()
// it.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return NewFromPool(pool), nil
}

// NewFromPool wraps an existing pool (useful in tests).
func NewFromPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, notify: newNotifier(pool), Recheck: 5 * time.Second}
}

// Ping checks that PostgreSQL answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close releases the underlying pool.
func (s *Store) Close() {
	s.notify.close()
	s.pool.Close()
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
		WITH created AS (
			INSERT INTO sessions (session_id, tenant_id, created_by, title, project, status, metadata, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$9,$5,$6,$7,$8)
			RETURNING session_id, tenant_id, created_by, created_at)
		INSERT INTO session_participants (session_id, tenant_id, principal_id, role, joined_at)
		SELECT session_id, tenant_id, created_by, 'owner', created_at FROM created`,
		string(sess.ID), string(sess.TenantID), string(sess.CreatedBy),
		sess.Title, sess.Status, encJSON(sess.Metadata), sess.CreatedAt, sess.UpdatedAt, sess.Project,
	)
	if err != nil {
		return "", mapErr(err)
	}
	return sess.ID, nil
}

func (s *Store) GetSession(ctx context.Context, tenant core.TenantID, id core.SessionID) (store.Session, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT session_id, tenant_id, created_by, title, project, status, metadata, created_at, updated_at
		FROM sessions WHERE session_id=$1 AND tenant_id=$2`,
		string(id), string(tenant))
	var out store.Session
	var meta []byte
	var sid, tid, cb string
	if err := row.Scan(&sid, &tid, &cb, &out.Title, &out.Project, &out.Status, &meta, &out.CreatedAt, &out.UpdatedAt); err != nil {
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

func (s *Store) ListSessions(ctx context.Context, tenant core.TenantID, participant core.PrincipalID) ([]store.Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, tenant_id, created_by, title, project, status, metadata, created_at, updated_at
		FROM sessions s WHERE tenant_id=$1
		  AND ($2::uuid IS NULL OR EXISTS (
		      SELECT 1 FROM session_participants p WHERE p.session_id = s.session_id AND p.principal_id = $2::uuid))
		ORDER BY created_at DESC`, string(tenant), nullableUUID(string(participant)))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Session{}
	for rows.Next() {
		var sess store.Session
		var meta []byte
		var sid, tid, createdBy string
		if err := rows.Scan(&sid, &tid, &createdBy, &sess.Title, &sess.Project, &sess.Status, &meta, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
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

func (s *Store) PutParticipant(ctx context.Context, p store.Participant) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO session_participants (session_id, tenant_id, principal_id, role)
		SELECT session_id, tenant_id, $3, $4 FROM sessions WHERE session_id=$1 AND tenant_id=$2
		ON CONFLICT (session_id, principal_id) DO UPDATE SET role = EXCLUDED.role`,
		string(p.SessionID), string(p.TenantID), string(p.PrincipalID), p.Role)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

const participantColumns = `tenant_id, session_id, principal_id, role, joined_at`

func scanParticipant(row rowScanner) (store.Participant, error) {
	var p store.Participant
	var tid, sid, pid string
	if err := row.Scan(&tid, &sid, &pid, &p.Role, &p.JoinedAt); err != nil {
		return store.Participant{}, mapErr(err)
	}
	p.TenantID, p.SessionID, p.PrincipalID = core.TenantID(tid), core.SessionID(sid), core.PrincipalID(pid)
	return p, nil
}

func (s *Store) GetParticipant(ctx context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) (store.Participant, error) {
	return scanParticipant(s.pool.QueryRow(ctx, `
		SELECT `+participantColumns+` FROM session_participants
		WHERE session_id=$1 AND tenant_id=$2 AND principal_id=$3`, string(session), string(tenant), string(principal)))
}

func (s *Store) ListParticipants(ctx context.Context, tenant core.TenantID, session core.SessionID) ([]store.Participant, error) {
	if _, err := s.GetSession(ctx, tenant, session); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+participantColumns+` FROM session_participants
		WHERE session_id=$1 AND tenant_id=$2 ORDER BY joined_at, principal_id`, string(session), string(tenant))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Participant{}
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) RemoveParticipant(ctx context.Context, tenant core.TenantID, session core.SessionID, principal core.PrincipalID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM session_participants WHERE session_id=$1 AND tenant_id=$2 AND principal_id=$3`,
		string(session), string(tenant), string(principal))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
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
		m.Role, string(m.Visibility), s.Sealer.text("messages.content", m.Content), m.CreatedAt,
	)
	return mapErr(err)
}

func (s *Store) ListMessages(ctx context.Context, tenant core.TenantID, id core.SessionID) ([]store.MessageProjection, error) {
	if _, err := s.GetSession(ctx, tenant, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT message_id, session_id, run_id, event_id, role, visibility, content, created_at
		FROM messages WHERE session_id=$1 ORDER BY created_at, message_id`, string(id))
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
		if m.Content, err = s.Sealer.openText("messages.content", m.Content); err != nil {
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
		                  workflow, input, output, queued_at, started_at, ended_at, started_by, updated_at,
		                  owner, lease_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now(),
		        $17, CASE WHEN $17::text IS NULL THEN NULL ELSE now() + make_interval(secs => $18) END)`,
		string(r.ID), string(r.TenantID), string(r.SessionID), nullableUUID(string(r.ParentRunID)),
		string(r.Status), limits, usage, r.FailureReason, nullable(r.IdempotencyKey),
		nullableJSON(r.Workflow), nullableJSON(s.Sealer.json("runs.input", r.Input)), nullableJSON(s.Sealer.json("runs.output", r.Output)),
		r.QueuedAt, nullableTime(r.StartedAt), nullableTime(r.EndedAt), nullableUUID(string(r.StartedBy)),
		nullable(r.Owner), time.Until(r.LeaseUntil).Seconds(),
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
		       queued_at, started_at, ended_at, started_by, owner, lease_until, cancel_requested
		FROM runs WHERE run_id=$1 AND tenant_id=$2`, string(id), string(tenant))
	return s.scanRun(row)
}

func (s *Store) ClaimRun(ctx context.Context, tenant core.TenantID, id core.RunID, owner string, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET owner=$3, lease_until=now() + make_interval(secs => $4)
		WHERE run_id=$1 AND tenant_id=$2 AND (owner IS NULL OR owner=$3 OR lease_until < now())`,
		string(id), string(tenant), owner, lease.Seconds())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetRun(ctx, tenant, id); err != nil {
			return err
		}
		return fmt.Errorf("run %s is held by another process: %w", id, core.ErrConflict)
	}
	return nil
}

func (s *Store) RenewLeases(ctx context.Context, owner string, runs []core.RunID, lease time.Duration) (map[core.RunID]bool, error) {
	ids := make([]string, len(runs))
	for i, id := range runs {
		ids[i] = string(id)
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE runs SET lease_until=now() + make_interval(secs => $3)
		WHERE owner=$1 AND run_id = ANY($2::uuid[])
		RETURNING run_id, cancel_requested`, owner, ids, lease.Seconds())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	held := map[core.RunID]bool{}
	for rows.Next() {
		var id string
		var cancel bool
		if err := rows.Scan(&id, &cancel); err != nil {
			return nil, err
		}
		held[core.RunID(id)] = cancel
	}
	return held, mapErr(rows.Err())
}

func (s *Store) ReleaseRun(ctx context.Context, id core.RunID, owner string) error {
	_, err := s.pool.Exec(ctx, `UPDATE runs SET owner=NULL, lease_until=NULL WHERE run_id=$1 AND owner=$2`, string(id), owner)
	return mapErr(err)
}

func (s *Store) RequestCancel(ctx context.Context, tenant core.TenantID, id core.RunID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET cancel_requested=true WHERE run_id=$1 AND tenant_id=$2`, string(id), string(tenant))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
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
		r.FailureReason, nullableJSON(r.Workflow), nullableJSON(s.Sealer.json("runs.input", r.Input)), nullableJSON(s.Sealer.json("runs.output", r.Output)),
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

func (s *Store) UpdateRunStatus(ctx context.Context, tenant core.TenantID, id core.RunID, status core.RunStatus, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status=$3, failure_reason=$4,
		                started_at=CASE WHEN $3='running' THEN coalesce(started_at, now()) ELSE started_at END,
		                updated_at=now()
		WHERE run_id=$1 AND tenant_id=$2`,
		string(id), string(tenant), string(status), reason)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateRunUsage(ctx context.Context, tenant core.TenantID, id core.RunID, usage core.Usage) error {
	raw, _ := json.Marshal(usage)
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET usage_totals=$3, updated_at=now() WHERE run_id=$1 AND tenant_id=$2`,
		string(id), string(tenant), raw)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, tenant core.TenantID, sessionID core.SessionID) ([]store.Run, error) {
	return s.queryRuns(ctx, `WHERE tenant_id=$1 AND session_id=$2`, string(tenant), string(sessionID))
}

func (s *Store) ListUnfinishedRuns(ctx context.Context) ([]store.Run, error) {
	return s.queryRuns(ctx, `WHERE status NOT IN ('succeeded', 'failed', 'canceled', 'timed_out')`)
}

func (s *Store) queryRuns(ctx context.Context, where string, args ...any) ([]store.Run, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, tenant_id, session_id, parent_run_id, status, limits, usage_totals,
		       failure_reason, idempotency_key, workflow, input, output,
		       queued_at, started_at, ended_at, started_by, owner, lease_until, cancel_requested
		FROM runs `+where+` ORDER BY queued_at`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Run{}
	var unreadable []error
	for rows.Next() {
		r, err := s.scanRun(rows)
		if errors.Is(err, ErrSealed) || errors.Is(err, errCorrupt) {
			unreadable = append(unreadable, err)
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, errors.Join(unreadable...)
}

func (s *Store) FindByIdempotency(ctx context.Context, tenant core.TenantID, key string) (store.Run, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT run_id, tenant_id, session_id, parent_run_id, status, limits, usage_totals,
		       failure_reason, idempotency_key, workflow, input, output,
		       queued_at, started_at, ended_at, started_by, owner, lease_until, cancel_requested
		FROM runs WHERE tenant_id=$1 AND idempotency_key=$2`, string(tenant), key)
	return s.scanRun(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanRun(row rowScanner) (store.Run, error) {
	var (
		out                  store.Run
		rid, tid, sid        string
		parent, idem         *string
		limits, usage        []byte
		workflow, in, output []byte
		started, ended       *time.Time
		status               string
		startedBy            *string
		owner                *string
		leaseUntil           *time.Time
	)
	if err := row.Scan(&rid, &tid, &sid, &parent, &status, &limits, &usage,
		&out.FailureReason, &idem, &workflow, &in, &output,
		&out.QueuedAt, &started, &ended, &startedBy, &owner, &leaseUntil, &out.CancelRequested); err != nil {
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
	if startedBy != nil {
		out.StartedBy = core.PrincipalID(*startedBy)
	}
	if owner != nil {
		out.Owner = *owner
	}
	if leaseUntil != nil {
		out.LeaseUntil = *leaseUntil
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
	var err error
	if out.Input, err = s.Sealer.openJSON("runs.input", in); err != nil {
		return store.Run{}, err
	}
	if out.Output, err = s.Sealer.openJSON("runs.output", output); err != nil {
		return store.Run{}, err
	}
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
		nullableJSONOr(s.Sealer.json("tasks.input", t.Input), "{}"), nullableJSON(s.Sealer.json("tasks.output", t.Output)), t.Error, t.CreatedAt,
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
		nullableJSON(s.Sealer.json("tasks.output", t.Output)), t.Error,
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
		if t.Input, err = s.Sealer.openJSON("tasks.input", input); err != nil {
			return nil, err
		}
		if t.Output, err = s.Sealer.openJSON("tasks.output", outp); err != nil {
			return nil, err
		}
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
		INSERT INTO approvals (approval_id, tenant_id, run_id, step_id, reason, status, request, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		string(a.ID), string(a.TenantID), nullableUUID(string(a.RunID)), nullableUUID(string(a.StepID)),
		a.Reason, a.Status, s.Sealer.json("approvals.request", encJSON(a.Request)), a.CreatedAt, nullableTime(a.ExpiresAt),
	)
	if err != nil {
		return "", mapErr(err)
	}
	return a.ID, nil
}

const approvalColumns = `approval_id, tenant_id, run_id, step_id, reason, status, request, resolution, created_at, resolved_at, expires_at, resolved_by`

func (s *Store) scanApproval(row rowScanner) (store.Approval, error) {
	var (
		a                 store.Approval
		aid, tid          string
		rid, sid, by      *string
		req, res          []byte
		resolved, expires *time.Time
	)
	if err := row.Scan(&aid, &tid, &rid, &sid, &a.Reason, &a.Status, &req, &res, &a.CreatedAt, &resolved, &expires, &by); err != nil {
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
	var err error
	if a.Request, err = s.Sealer.openJSON("approvals.request", req); err != nil {
		return store.Approval{}, err
	}
	if a.Resolution, err = s.Sealer.openJSON("approvals.resolution", res); err != nil {
		return store.Approval{}, err
	}
	if resolved != nil {
		a.ResolvedAt = *resolved
	}
	if expires != nil {
		a.ExpiresAt = *expires
	}
	if by != nil {
		a.ResolvedBy = core.PrincipalID(*by)
	}
	return a, nil
}

func (s *Store) GetApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (store.Approval, error) {
	return s.scanApproval(s.pool.QueryRow(ctx, `
		SELECT `+approvalColumns+` FROM approvals WHERE approval_id=$1 AND tenant_id=$2`, string(id), string(tenant)))
}

func (s *Store) ResolveApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string, by core.PrincipalID, resolution json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `
		WITH resolved AS (
			UPDATE approvals SET status=$3, resolution=$4, resolved_at=now(), resolved_by=$5
			WHERE approval_id=$1 AND tenant_id=$2 AND status='requested'
			  AND ($3 IN ('expired', 'canceled') OR expires_at IS NULL OR expires_at > now())
			RETURNING approval_id)
		SELECT pg_notify('`+approvalsChannel+`', approval_id::text) FROM resolved`,
		string(id), string(tenant), status, nullableJSON(s.Sealer.json("approvals.resolution", resolution)), nullableUUID(string(by)),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		// Not found, already resolved, or expired.
		if _, gerr := s.GetApproval(ctx, tenant, id); gerr != nil {
			return gerr
		}
		return core.ErrConflict
	}
	return nil
}

func (s *Store) CancelRunApprovals(ctx context.Context, tenant core.TenantID, run core.RunID) error {
	_, err := s.pool.Exec(ctx, `
		WITH canceled AS (
			UPDATE approvals SET status='canceled', resolved_at=now()
			WHERE tenant_id=$1 AND run_id=$2 AND status='requested'
			RETURNING approval_id)
		SELECT pg_notify('`+approvalsChannel+`', approval_id::text) FROM canceled`, string(tenant), string(run))
	return mapErr(err)
}

func (s *Store) ListApprovals(ctx context.Context, tenant core.TenantID, f store.ApprovalFilter) ([]store.Approval, error) {
	limit := any(nil)
	if f.Limit > 0 {
		limit = f.Limit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+approvalColumns+` FROM approvals
		WHERE tenant_id=$1 AND ($2::uuid IS NULL OR run_id = $2::uuid) AND ($3 = '' OR status = $3)
		  AND ($3 <> 'requested' OR expires_at IS NULL OR expires_at > now())
		  AND ($4::uuid IS NULL OR approval_id > $4::uuid)
		  AND ($6::uuid IS NULL OR run_id IN (
		      SELECT r.run_id FROM runs r JOIN session_participants p ON p.session_id = r.session_id
		      WHERE r.tenant_id = $1 AND p.tenant_id = $1 AND p.principal_id = $6::uuid))
		ORDER BY approval_id
		LIMIT $5`, string(tenant), nullableUUID(string(f.RunID)), f.Status, nullableUUID(string(f.After)), limit, nullableUUID(string(f.Participant)))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Approval{}
	for rows.Next() {
		a, err := s.scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

// WaitForApproval waits until the approval resolves or ctx is canceled.
func (s *Store) WaitForApproval(ctx context.Context, id core.ApprovalID) (store.Approval, error) {
	wake, unwatch := s.notify.watch(approvalsChannel, string(id))
	defer unwatch()
	t := time.NewTicker(s.Recheck)
	defer t.Stop()
	for {
		a, err := s.scanApproval(s.pool.QueryRow(ctx, `
			SELECT `+approvalColumns+` FROM approvals WHERE approval_id=$1`, string(id)))
		if err != nil {
			return store.Approval{}, err
		}
		if a.Status != "requested" {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return store.Approval{}, ctx.Err()
		case <-wake:
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", mapErr(err)
	}
	defer tx.Rollback(ctx)
	// Puts of one name take turns, so each reads the version the one before it wrote.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || '/' || $2, 0))`, string(a.SessionID), a.Name); err != nil {
		return "", mapErr(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO artifacts (`+artifactColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,
		        (SELECT COALESCE(max(version), 0) + 1 FROM artifacts WHERE session_id=$3 AND name=$9),
		        $10,$11,$12,$13,$14)`,
		a.ID, string(a.TenantID), string(a.SessionID), nullableUUID(string(a.RunID)), nullableUUID(string(a.TaskID)),
		nullableUUID(string(a.StepID)), a.Producer, a.Kind, a.Name, a.MimeType, a.Size, s.Sealer.bytes("artifacts.content", a.Content), encJSON(a.Metadata), a.CreatedAt,
	); err != nil {
		return "", mapErr(err)
	}
	return a.ID, mapErr(tx.Commit(ctx))
}

const artifactColumns = `artifact_id, tenant_id, session_id, run_id, task_id, step_id, producer, kind, name, version, mime_type, size_bytes, content, metadata, created_at`

// artifactListColumns are artifactColumns without the content, which lists leave out.
const artifactListColumns = `artifact_id, tenant_id, session_id, run_id, task_id, step_id, producer, kind, name, version, mime_type, size_bytes, NULL::bytea, metadata, created_at`

func (s *Store) scanArtifact(row rowScanner) (store.Artifact, error) {
	var (
		a                   store.Artifact
		tid, sid            string
		rid, taskID, stepID *string
		meta                []byte
	)
	if err := row.Scan(&a.ID, &tid, &sid, &rid, &taskID, &stepID, &a.Producer, &a.Kind, &a.Name, &a.Version, &a.MimeType, &a.Size, &a.Content, &meta, &a.CreatedAt); err != nil {
		return store.Artifact{}, mapErr(err)
	}
	var err error
	if a.Content, err = s.Sealer.openBytes("artifacts.content", a.Content); err != nil {
		return store.Artifact{}, err
	}
	a.TenantID, a.SessionID = core.TenantID(tid), core.SessionID(sid)
	if rid != nil {
		a.RunID = core.RunID(*rid)
	}
	if taskID != nil {
		a.TaskID = core.TaskID(*taskID)
	}
	if stepID != nil {
		a.StepID = core.StepID(*stepID)
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &a.Metadata)
	}
	return a, nil
}

func (s *Store) GetArtifact(ctx context.Context, tenant core.TenantID, id string) (store.Artifact, error) {
	return s.scanArtifact(s.pool.QueryRow(ctx, `
		SELECT `+artifactColumns+` FROM artifacts WHERE artifact_id=$1 AND tenant_id=$2`, id, string(tenant)))
}

func (s *Store) ListRunArtifacts(ctx context.Context, tenant core.TenantID, run core.RunID) ([]store.Artifact, error) {
	return s.queryArtifacts(ctx, `
		SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id=$1 AND run_id=$2 ORDER BY created_at, artifact_id`,
		string(tenant), string(run))
}

func (s *Store) ListArtifactVersions(ctx context.Context, tenant core.TenantID, session core.SessionID, name string) ([]store.Artifact, error) {
	return s.queryArtifacts(ctx, `
		SELECT `+artifactListColumns+` FROM artifacts WHERE tenant_id=$1 AND session_id=$2 AND name=$3 ORDER BY version`,
		string(tenant), string(session), name)
}

func (s *Store) queryArtifacts(ctx context.Context, q string, args ...any) ([]store.Artifact, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.Artifact{}
	for rows.Next() {
		a, err := s.scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
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
	var sessions []string
	for _, ev := range events {
		if !slices.Contains(sessions, string(ev.SessionID)) {
			sessions = append(sessions, string(ev.SessionID))
		}
	}
	if err := lockSessions(ctx, tx, sessions); err != nil {
		return nil, err
	}

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
			s.Sealer.json("run_events.payload", encJSON(ev.Payload)), nullableJSON(s.Sealer.json("run_events.hidden_payload", ev.HiddenPayload)),
			ev.OccurredAt, ev.RecordedAt,
		)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, ev)
	}
	var runs []string
	for _, ev := range out {
		if ev.RunID != "" && !slices.Contains(runs, string(ev.RunID)) {
			runs = append(runs, string(ev.RunID))
		}
	}
	if len(runs) > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_notify('`+eventsChannel+`', r) FROM unnest($1::text[]) r`, runs); err != nil {
			return nil, mapErr(err)
		}
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
		if ev.Payload, err = s.Sealer.openJSON("run_events.payload", payload); err != nil {
			return nil, err
		}
		if ev.HiddenPayload, err = s.Sealer.openJSON("run_events.hidden_payload", hidden); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// Subscribe delivers a run's events after afterSeq as notifications arrive. It
// returns a channel and a cancel func; the channel is closed when the cancel
// func is invoked or the context is canceled.
func (s *Store) Subscribe(ctx context.Context, run core.RunID, afterSeq int64) (<-chan core.Event, func()) {
	ch := make(chan core.Event, 64)
	subCtx, cancel := context.WithCancel(ctx)
	wake, unwatch := s.notify.watch(eventsChannel, string(run))
	go func() {
		defer close(ch)
		defer unwatch()
		seq := afterSeq
		t := time.NewTicker(s.Recheck)
		defer t.Stop()
		for {
			events, err := s.queryEvents(subCtx, `
				SELECT event_id, tenant_id, session_id, run_id, sequence, session_sequence,
				       event_type, visibility, actor_id, task_id, step_id, parent_event_id,
				       correlation_id, causation_id, payload, hidden_payload,
				       occurred_at, recorded_at
				FROM run_events WHERE run_id=$1 AND sequence>$2 ORDER BY sequence`,
				string(run), seq)
			if err != nil {
				return
			}
			for _, ev := range events {
				select {
				case ch <- ev:
					seq = ev.Sequence
				case <-subCtx.Done():
					return
				}
			}
			select {
			case <-subCtx.Done():
				return
			case <-wake:
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

// lockSessions takes a key-share lock on sessions, which a purge waits for and
// which waits for a purge, so nothing is written into a session being deleted;
// it fails with core.ErrNotFound when one is gone.
func lockSessions(ctx context.Context, tx pgx.Tx, sessions []string) error {
	rows, err := tx.Query(ctx, `SELECT session_id FROM sessions WHERE session_id = ANY($1::uuid[]) FOR KEY SHARE`, sessions)
	if err != nil {
		return mapErr(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}
	if n < len(sessions) {
		return fmt.Errorf("session: %w", core.ErrNotFound)
	}
	return nil
}
