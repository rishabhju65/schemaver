package diff_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/shadow"
)

// TestDiffAgainstRealSchemas exercises the engine on schemas an engine actually
// produced, rather than on models assembled by hand.
//
// This is the difference that matters: hand-built models exercise the diff
// logic, but only introspected ones exercise it against the shapes PostgreSQL
// really reports — qualified type names, deparsed constraint definitions,
// implicit sequences behind identity columns. A diff that works on the former
// and not the latter would report phantom changes on every real database.
func TestDiffAgainstRealSchemas(t *testing.T) {
	dsn := os.Getenv("SCHEMAVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SCHEMAVER_TEST_DATABASE_URL to run the diff integration test")
	}
	pool, err := shadow.NewPool(dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const before = `
		CREATE TYPE order_status AS ENUM ('pending', 'paid');
		CREATE TABLE customers (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email text NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_id bigint NOT NULL REFERENCES customers (id),
			status order_status NOT NULL DEFAULT 'pending',
			legacy_note text
		);
		CREATE INDEX orders_customer_idx ON orders (customer_id);`

	const after = `
		CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
		CREATE TABLE customers (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email text NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_id bigint NOT NULL REFERENCES customers (id),
			status order_status NOT NULL DEFAULT 'pending',
			total numeric(12,2) NOT NULL DEFAULT 0,
			note text
		);
		CREATE INDEX orders_customer_idx ON orders (customer_id);
		CREATE INDEX orders_total_idx ON orders (total);`

	from, _, err := pool.Load(ctx, before)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}
	to, _, err := pool.Load(ctx, after)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}

	// A schema against itself must be silent, or every real diff carries noise.
	if r := diff.Compute(from, from); !r.Empty() {
		t.Errorf("a real schema differs from itself in %d ways: %+v", len(r.Changes), r.Changes)
	}

	r := diff.Compute(from, to)
	if r.Empty() {
		t.Fatal("no changes found between two different schemas")
	}

	found := map[diff.Kind]bool{}
	for _, c := range r.Changes {
		found[c.Kind] = true
		t.Logf("  [%s] %s", c.Class, c.Summary)
	}
	for _, want := range []diff.Kind{
		diff.AddColumn,    // total, note
		diff.DropColumn,   // legacy_note
		diff.AddEnumLabel, // shipped
		diff.CreateIndex,  // orders_total_idx
	} {
		if !found[want] {
			t.Errorf("%s not detected", want)
		}
	}

	// legacy_note out, note in, both text: exactly the ambiguity a human must
	// resolve.
	var proposed bool
	for _, c := range r.Renames {
		if c.From == "legacy_note" && c.To == "note" {
			proposed = true
			t.Logf("  rename candidate (%s): %s", c.Confidence, c.Question)
		}
	}
	if !proposed {
		t.Error("legacy_note → note was not proposed as a possible rename")
	}

	// Reversing the diff must be non-empty and classified differently: dropping
	// what was added is destructive.
	back := diff.Compute(to, from)
	if back.Summary.Destructive == 0 {
		t.Error("reversing the diff reports nothing destructive, though it drops columns")
	}
}
