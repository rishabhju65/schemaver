// Package migrate applies schemaver's own metadata migrations.
//
// This is deliberately not the migration engine the product offers its users —
// that one is Phase 2 and does not exist yet. This is the plain bootstrap that
// gets schemaver's own tables in place. Once the real executor exists, the
// metadata schema becomes the first thing it manages: the one database where a
// bug costs us rather than a user.
package migrate

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var files embed.FS

// Apply runs every migration that has not yet been applied, in filename order.
func Apply(ctx context.Context, pool *pgxpool.Pool) (applied []string, err error) {
	// The tracking table cannot itself be created by a migration, so the runner
	// owns it.
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS schemaver;
		CREATE TABLE IF NOT EXISTS schemaver.schema_migration (
		    version    text PRIMARY KEY,
		    checksum   text        NOT NULL,
		    applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create migration tracking table: %w", err)
	}

	entries, err := files.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := files.ReadFile("sql/" + name)
		if err != nil {
			return applied, fmt.Errorf("read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		var existing string
		err = pool.QueryRow(ctx,
			`SELECT checksum FROM schemaver.schema_migration WHERE version = $1`, name).
			Scan(&existing)
		if err == nil {
			// Applied already. A changed checksum means history was edited after
			// the fact, which is exactly the silent divergence the product exists
			// to catch — so refuse rather than guess.
			if existing != checksum {
				return applied, fmt.Errorf(
					"migration %s was modified after it was applied (checksum %s, expected %s)",
					name, checksum[:12], existing[:12])
			}
			continue
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schemaver.schema_migration (version, checksum) VALUES ($1, $2)`,
			name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("commit %s: %w", name, err)
		}
		applied = append(applied, name)
	}
	return applied, nil
}
