package pg

import (
	"context"
	"strconv"
	"time"
)

// purgeBatch is how many sessions one purge transaction deletes at most.
const purgeBatch = 500

// idleSessions selects sessions created before $1 with no event and no
// session memory since $1, and no unfinished run.
const idleSessions = `
	SELECT s.session_id::text FROM sessions s
	WHERE s.created_at < $1
	  AND NOT EXISTS (SELECT 1 FROM run_events e WHERE e.session_id = s.session_id AND e.recorded_at >= $1)
	  AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.scope = 'session' AND m.session_id = s.session_id AND m.created_at >= $1)
	  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.session_id = s.session_id
	                  AND r.status NOT IN ('succeeded', 'failed', 'canceled', 'timed_out'))`

func (s *Store) PurgeSessions(ctx context.Context, idleSince time.Time) (int, error) {
	total := 0
	for {
		n, err := s.purgeBatch(ctx, idleSince)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// purgeBatch deletes up to purgeBatch idle sessions in one transaction.
func (s *Store) purgeBatch(ctx context.Context, idleSince time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, mapErr(err)
	}
	defer tx.Rollback(ctx)
	var locked, sessions, runs []string
	// Locking the sessions makes writers into them (Append, CreateRun, session
	// memories) wait and then find them gone; the second, fresh read drops a
	// session that a writer committed to before the lock was taken.
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(session_id), '{}') FROM (`+idleSessions+`
		LIMIT `+strconv.Itoa(purgeBatch)+` FOR UPDATE OF s SKIP LOCKED) idle`, idleSince).Scan(&locked); err != nil {
		return 0, mapErr(err)
	}
	if len(locked) == 0 {
		return 0, nil
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(session_id), '{}') FROM (`+idleSessions+`
		AND s.session_id = ANY($2::uuid[])) idle`, idleSince, locked).Scan(&sessions); err != nil {
		return 0, mapErr(err)
	}
	if len(sessions) == 0 {
		return 0, mapErr(tx.Commit(ctx))
	}
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(array_agg(run_id::text), '{}') FROM runs WHERE session_id = ANY($1::uuid[])`, sessions).Scan(&runs); err != nil {
		return 0, mapErr(err)
	}
	for _, q := range []string{
		`DELETE FROM run_events WHERE session_id = ANY($1::uuid[])`,
		`DELETE FROM memories WHERE scope = 'session' AND session_id = ANY($1::uuid[])`,
		`DELETE FROM event_counters WHERE scope_type = 'session' AND scope_id = ANY($1::uuid[])`,
		`DELETE FROM sessions WHERE session_id = ANY($1::uuid[])`,
	} {
		if _, err := tx.Exec(ctx, q, sessions); err != nil {
			return 0, mapErr(err)
		}
	}
	for _, q := range []string{
		`DELETE FROM approvals WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM event_counters WHERE scope_type = 'run' AND scope_id = ANY($1::uuid[])`,
	} {
		if _, err := tx.Exec(ctx, q, runs); err != nil {
			return 0, mapErr(err)
		}
	}
	return len(sessions), mapErr(tx.Commit(ctx))
}

func (s *Store) PurgeMemories(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE created_at < $1`, before)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}
