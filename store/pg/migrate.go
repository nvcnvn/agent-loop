package pg

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// migrations returns the embedded migrations, which must be named
// 0001_<name>.sql, 0002_<name>.sql, … with no gaps.
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		num, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		version, err := strconv.Atoi(num)
		if !ok || err != nil || version != len(out)+1 || name == "" {
			return nil, fmt.Errorf("migration %s: want %04d_<name>.sql", e.Name(), len(out)+1)
		}
		sql, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, sql: string(sql)})
	}
	return out, nil
}

// Migrate applies, in one transaction, the embedded migrations newer than the
// latest version recorded in schema_migrations.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Replicas starting together would otherwise apply the same migration twice.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent-loop schema_migrations'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	// A newer binary may have migrated further; see the migration contract in docs/04.
	for _, m := range ms[min(current, len(ms)):] {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
