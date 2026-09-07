package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/shadow"
)

// TestGeneratedMigrationsProduceTheTargetSchema is the test this whole project
// has been building towards.
//
// For each pair of schemas: diff them, render the migration, apply it to a
// database built at the source, and require the result to fingerprint as the
// target. That is D-009's shadow proof used as a test of the generator — if a
// generated migration does not produce the schema it claims, this fails, and
// nothing downstream can be trusted until it passes.
func TestGeneratedMigrationsProduceTheTargetSchema(t *testing.T) {
	dsn := os.Getenv("SCHEMAVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SCHEMAVER_TEST_DATABASE_URL to run migration generation tests")
	}
	pool, err := shadow.NewPool(dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const start = `
		CREATE TYPE order_status AS ENUM ('pending', 'paid');
		CREATE TABLE customers (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email text NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_id bigint NOT NULL REFERENCES customers (id),
			status order_status NOT NULL DEFAULT 'pending',
			legacy_note text,
			total numeric(10,2)
		);
		CREATE INDEX orders_customer_idx ON orders (customer_id);
		COMMENT ON TABLE orders IS 'orders';`

	cases := []struct {
		name   string
		target string
	}{
		{"add a nullable column", start + `ALTER TABLE orders ADD COLUMN note text;`},
		{"drop a column", strings.Replace(start, "\t\t\tlegacy_note text,\n", "", 1)},
		{"widen a numeric type", strings.Replace(start, "numeric(10,2)", "numeric(14,4)", 1)},
		{"require non-null", strings.Replace(start, "total numeric(10,2)", "total numeric(10,2) NOT NULL DEFAULT 0", 1)},
		{"append an enum value", strings.Replace(start, "'pending', 'paid'", "'pending', 'paid', 'shipped'", 1)},
		{"add an index", start + `CREATE INDEX orders_total_idx ON orders (total);`},
		{"add a check constraint", start + `ALTER TABLE orders ADD CONSTRAINT orders_total_positive CHECK (total >= 0);`},
		{"add a table with a foreign key", start + `
			CREATE TABLE shipments (
				id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				order_id bigint NOT NULL REFERENCES orders (id) ON DELETE CASCADE
			);
			CREATE INDEX shipments_order_idx ON shipments (order_id);`},
		{"drop a table", strings.Replace(start,
			"CREATE INDEX orders_customer_idx ON orders (customer_id);", "", 1)},
		{"change a comment", strings.Replace(start, "'orders'", "'Customer orders'", 1)},
		{"a new schema", start + `
			CREATE SCHEMA reporting;
			CREATE TABLE reporting.daily (day date PRIMARY KEY, total numeric(14,2));`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, _, err := pool.Load(ctx, start)
			if err != nil {
				t.Fatalf("load source: %v", err)
			}
			to, want, err := pool.Load(ctx, tc.target)
			if err != nil {
				t.Fatalf("load target: %v", err)
			}

			result := diff.Compute(from, to)
			if result.Empty() {
				t.Fatal("no changes detected between different schemas")
			}
			statements := render.Statements(result.Changes, from, to)
			script := render.SQL(statements)

			for _, s := range statements {
				if strings.HasPrefix(strings.TrimSpace(s.SQL), "-- MANUAL") {
					t.Fatalf("a change could not be generated:\n%s", s.SQL)
				}
			}

			// Concurrent index builds cannot run inside a transaction, and
			// shadow.Apply sends the batch as one. Verify them separately.
			atomic := true
			for _, s := range statements {
				if !s.Transactional {
					atomic = false
				}
			}
			if !atomic {
				t.Logf("contains a non-transactional statement; verifying statement by statement")
				got, err := applyStepwise(ctx, pool, start, statements)
				if err != nil {
					t.Fatalf("applying generated migration failed: %v\n\n%s", err, script)
				}
				if got != want {
					t.Errorf("migration produced %s, want %s\n\n%s", got.Short(), want.Short(), script)
				}
				return
			}

			got, err := pool.Verify(ctx, start, script, want)
			if err != nil {
				t.Fatalf("%v\n\n--- generated ---\n%s", err, script)
			}
			if got != want {
				t.Errorf("produced %s, want %s", got.Short(), want.Short())
			}
		})
	}
}

// applyStepwise builds the source schema then runs each statement on its own, so
// statements that refuse to run in a transaction can still be verified.
func applyStepwise(ctx context.Context, pool *shadow.Pool, base string, statements []render.Statement) (schema.Version, error) {
	db, err := pool.Create(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()

	if err := db.Apply(ctx, base); err != nil {
		return "", err
	}
	for _, s := range statements {
		if strings.HasPrefix(strings.TrimSpace(s.SQL), "--") {
			continue
		}
		if err := db.Apply(ctx, s.SQL); err != nil {
			return "", err
		}
	}
	got, err := db.Schema(ctx)
	if err != nil {
		return "", err
	}
	return schema.Fingerprint(got)
}
