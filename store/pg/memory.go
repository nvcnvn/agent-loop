package pg

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

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
	if err := m.Validate(); err != nil {
		return "", err
	}
	m.ID = uuidOrNew(m.ID)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memories (memory_id, tenant_id, scope, session_id, workflow, content, embedding, embed_model, source, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::vector,$8,$9,COALESCE($10, now()))`,
		m.ID, string(m.TenantID), string(m.Scope), nullableUUID(string(m.SessionID)), nullable(m.Workflow),
		m.Content, vectorLiteral(m.Embedding), m.EmbedModel, encJSON(m.Source), nullableTime(m.CreatedAt))
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
		SELECT memory_id, scope, session_id, workflow, content, embed_model, source, created_at,
		       1 - (embedding <=> $2::vector) AS score
		FROM memories
		WHERE tenant_id=$1 AND embed_model=$3 AND vector_dims(embedding)=$6
		  AND (scope='tenant' OR (scope='session' AND session_id=$4) OR (scope='workflow' AND workflow=$5))
		ORDER BY embedding <=> $2::vector
		LIMIT $7`,
		string(q.TenantID), vectorLiteral(q.Embedding), q.EmbedModel, nullableUUID(string(q.SessionID)), nullable(q.Workflow), len(q.Embedding), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.MemoryHit
	for rows.Next() {
		var h store.MemoryHit
		var scope string
		var sid, wf *string
		var src []byte
		if err := rows.Scan(&h.ID, &scope, &sid, &wf, &h.Content, &h.EmbedModel, &src, &h.CreatedAt, &h.Score); err != nil {
			return nil, err
		}
		h.TenantID, h.Scope = q.TenantID, store.MemoryScope(scope)
		if sid != nil {
			h.SessionID = core.SessionID(*sid)
		}
		if wf != nil {
			h.Workflow = *wf
		}
		if err := json.Unmarshal(src, &h.Source); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
