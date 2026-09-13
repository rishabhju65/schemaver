package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// storeSchema records a schema as a blob and returns its fingerprint, for
// fixtures that need a schema to exist without a database having been read at
// it.
func storeSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool, s *schema.Schema) string {
	t.Helper()
	fingerprint, err := schema.Fingerprint(s)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	canonical, err := schema.Canonical(s)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, 1, $3) ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, len(canonical)); err != nil {
		t.Fatalf("store blob: %v", err)
	}
	return string(fingerprint)
}
