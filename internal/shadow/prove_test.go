package shadow

import (
	"errors"
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// TestProveRecordsEachStep is the test that makes the fingerprint chain worth
// having: it checks not just that a migration reaches its target, but that the
// intermediate fingerprints are real and distinct.
//
// A chain that collapsed to the same value at every step would still satisfy
// "starts at from, ends at to" and would tell the executor nothing at all about
// where a half-applied migration stopped.
func TestProveRecordsEachStep(t *testing.T) {
	pool, ctx := testPool(t)

	base := `CREATE TABLE orders (id bigint PRIMARY KEY);`
	_, from, err := pool.Load(ctx, base)
	if err != nil {
		t.Fatalf("load base: %v", err)
	}

	steps := []string{
		`ALTER TABLE orders ADD COLUMN channel text;`,
		`ALTER TABLE orders ADD COLUMN total numeric;`,
		`CREATE INDEX orders_channel_idx ON orders (channel);`,
	}
	_, want, err := pool.Load(ctx, base+
		`ALTER TABLE orders ADD COLUMN channel text;
		 ALTER TABLE orders ADD COLUMN total numeric;
		 CREATE INDEX orders_channel_idx ON orders (channel);`)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}

	proof, err := pool.Prove(ctx, base, steps, from, want)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if len(proof.After) != len(steps) {
		t.Fatalf("got %d step fingerprints, want %d", len(proof.After), len(steps))
	}
	if proof.Final != want {
		t.Errorf("final %s, want %s", proof.Final.Short(), want.Short())
	}

	// Every step moved the schema somewhere new, and nowhere it had been.
	seen := map[schema.Version]int{proof.Base: 0}
	for i, fp := range proof.After {
		if at, ok := seen[fp]; ok {
			t.Errorf("statement %d left the schema at %s, already seen after statement %d",
				i+1, fp.Short(), at)
		}
		seen[fp] = i + 1
	}
}

// TestProveNamesTheFailingStatement checks the half of a failed proof that
// makes it useful: which statement, not merely that something went wrong.
func TestProveNamesTheFailingStatement(t *testing.T) {
	pool, ctx := testPool(t)

	base := `CREATE TABLE orders (id bigint PRIMARY KEY);`
	_, from, err := pool.Load(ctx, base)
	if err != nil {
		t.Fatalf("load base: %v", err)
	}

	steps := []string{
		`ALTER TABLE orders ADD COLUMN channel text;`,
		`ALTER TABLE nonexistent ADD COLUMN x text;`,
	}
	proof, err := pool.Prove(ctx, base, steps, from, from)

	var step *StepError
	if !errors.As(err, &step) {
		t.Fatalf("got %v, want a StepError", err)
	}
	if step.Ordinal != 2 {
		t.Errorf("blamed statement %d, want 2", step.Ordinal)
	}
	// What did apply is still reported, so a reader can see how far it got.
	if proof == nil || len(proof.After) != 1 {
		t.Errorf("the partial chain was lost: %+v", proof)
	}
}

// TestProveRejectsAnUnrebuildableBase distinguishes schemaver's own failure from
// the migration's. Sending somebody to rewrite a migration that was never the
// problem is the worst outcome a proof can have.
func TestProveRejectsAnUnrebuildableBase(t *testing.T) {
	pool, ctx := testPool(t)

	base := `CREATE TABLE orders (id bigint PRIMARY KEY);`
	wrong := schema.Version("0000000000000000000000000000000000000000000000000000000000000000")

	_, err := pool.Prove(ctx, base, []string{`ALTER TABLE orders ADD COLUMN x text;`},
		wrong, wrong)

	var bad *BaseError
	if !errors.As(err, &bad) {
		t.Fatalf("got %v, want a BaseError", err)
	}
	if bad.Got == "" {
		t.Error("the base error did not say what the schema rebuilt as")
	}
}
