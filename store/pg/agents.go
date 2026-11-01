package pg

import (
	"context"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

func (s *Store) PutAgent(ctx context.Context, a store.AgentDefinition) (int, error) {
	var version int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agents (tenant_id, name, definition) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, name) DO UPDATE
		SET definition = EXCLUDED.definition, version = agents.version + 1, updated_at = now()
		RETURNING version`, string(a.TenantID), a.Name, []byte(a.Definition)).Scan(&version)
	return version, mapErr(err)
}

func (s *Store) ListAgents(ctx context.Context, tenant core.TenantID) ([]store.AgentDefinition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, definition, version, updated_at FROM agents WHERE tenant_id=$1 ORDER BY name`, string(tenant))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []store.AgentDefinition{}
	for rows.Next() {
		a := store.AgentDefinition{TenantID: tenant}
		if err := rows.Scan(&a.Name, &a.Definition, &a.Version, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) DeleteAgent(ctx context.Context, tenant core.TenantID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM agents WHERE tenant_id=$1 AND name=$2`, string(tenant), name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}
