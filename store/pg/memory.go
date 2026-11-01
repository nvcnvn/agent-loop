package pg

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// vectorLiteral formats v as pgvector text input, "[1,2.5,3]".
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func (s *Store) PutMemory(ctx context.Context, m store.Memory) (string, error) {
	ids, err := s.PutMemories(ctx, []store.Memory{m})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

func (s *Store) PutMemories(ctx context.Context, ms []store.Memory) ([]string, error) {
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			return nil, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer tx.Rollback(ctx)
	var sessions []string
	for _, m := range ms {
		if m.Scope == store.MemorySession && !slices.Contains(sessions, string(m.SessionID)) {
			sessions = append(sessions, string(m.SessionID))
		}
	}
	if err := lockSessions(ctx, tx, sessions); err != nil {
		return nil, err
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		if ids[i], err = putMemory(ctx, tx, m); err != nil {
			return nil, err
		}
	}
	return ids, mapErr(tx.Commit(ctx))
}

func putMemory(ctx context.Context, tx pgx.Tx, m store.Memory) (string, error) {
	m.ID = uuidOrNew(m.ID)
	_, err := tx.Exec(ctx, `
		INSERT INTO memories (memory_id, tenant_id, scope, session_id, workflow, content, embedding, embed_model, source, created_at, project)
		VALUES ($1,$2,$3,$4,$5,$6,$7::vector,$8,$9,COALESCE($10, now()),$11)`,
		m.ID, string(m.TenantID), string(m.Scope), nullableUUID(string(m.SessionID)), nullable(m.Workflow),
		m.Content, vectorLiteral(m.Embedding), m.EmbedModel, encJSON(m.Source), nullableTime(m.CreatedAt), nullable(m.Project))
	return m.ID, mapErr(err)
}

// SearchMemories ranks by cosine distance (pgvector's <=>).
// ponytail: no ANN index, since the column has no fixed dimension; add an
// HNSW index on a per-model expression index when memories outgrow a scan.
func (s *Store) SearchMemories(ctx context.Context, q store.MemoryQuery) ([]store.MemoryHit, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT memory_id, scope, session_id, workflow, project, content, embed_model, source, created_at,
		       1 - (embedding <=> $2::vector) AS score
		FROM memories
		WHERE tenant_id=$1 AND embed_model=$3 AND vector_dims(embedding)=$6
		  AND (scope='tenant' OR (scope='session' AND session_id=$4) OR (scope='workflow' AND workflow=$5)
		       OR (scope='project' AND project=$8))
		ORDER BY embedding <=> $2::vector
		LIMIT $7`,
		string(q.TenantID), vectorLiteral(q.Embedding), q.EmbedModel, nullableUUID(string(q.SessionID)), nullable(q.Workflow), len(q.Embedding), limit, nullable(q.Project))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.MemoryHit
	for rows.Next() {
		var h store.MemoryHit
		var scope string
		var sid, wf, project *string
		var src []byte
		if err := rows.Scan(&h.ID, &scope, &sid, &wf, &project, &h.Content, &h.EmbedModel, &src, &h.CreatedAt, &h.Score); err != nil {
			return nil, err
		}
		h.TenantID, h.Scope = q.TenantID, store.MemoryScope(scope)
		if sid != nil {
			h.SessionID = core.SessionID(*sid)
		}
		if wf != nil {
			h.Workflow = *wf
		}
		if project != nil {
			h.Project = *project
		}
		if err := json.Unmarshal(src, &h.Source); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
