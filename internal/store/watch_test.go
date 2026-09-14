package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// twoSchemas builds a database holding two namespaces, the shape that makes
// whole-database watching wrong: one team's schema and somebody else's.
func twoSchemas() *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{
		{Name: "public", Tables: []schema.Table{{Name: "orders",
			Columns: []schema.Column{{Name: "id", Type: "bigint"}}}}},
		{Name: "reporting", Tables: []schema.Table{{Name: "daily",
			Columns: []schema.Column{{Name: "day", Type: "date"}}}}},
	}}
}

// TestADatabaseWatchesEverythingUntilToldOtherwise is the default, and it is
// the safe direction: a schema nobody has ruled on is watched, so it shows up
// as a difference rather than as silence.
func TestADatabaseWatchesEverythingUntilToldOtherwise(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, twoSchemas())
	scope := st.ForProject(projectID)

	got, err := scope.Namespaces(ctx, databaseID)
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both schemas, got %d", len(got))
	}
	for _, ns := range got {
		if !ns.Watched {
			t.Errorf("%s is not watched and nobody said so", ns.Name)
		}
		if ns.Objects == 0 {
			t.Errorf("%s reports no objects; a reader cannot tell whether an "+
				"unfamiliar name matters", ns.Name)
		}
	}
}

// TestExcludingASchemaIsRememberedAsAnExclusion covers the direction of the
// stored list, which is the decision that matters.
//
// An include-list is a closed world: a schema created after somebody chose —
// a tenant onboarded, an extension installed — is silently unwatched, and a
// report of "nothing changed" that means "nothing was looked at" is the exact
// failure this product exists to prevent.
func TestExcludingASchemaIsRememberedAsAnExclusion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, twoSchemas())
	scope := st.ForProject(projectID)

	if err := scope.WatchNamespaces(ctx, userID, databaseID, []string{"reporting"}); err != nil {
		t.Fatalf("WatchNamespaces: %v", err)
	}

	var stored []string
	if err := pool.QueryRow(ctx,
		`SELECT excluded_namespaces FROM schemaver.database WHERE id = $1`, databaseID).
		Scan(&stored); err != nil {
		t.Fatalf("read what was stored: %v", err)
	}
	if len(stored) != 1 || stored[0] != "reporting" {
		t.Errorf("stored %v; the list must be what is excluded, not what is kept", stored)
	}

	// The excluded one is still offered, or un-excluding it would mean
	// remembering its name.
	got, err := scope.Namespaces(ctx, databaseID)
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	var sawExcluded bool
	for _, ns := range got {
		if ns.Name == "reporting" {
			sawExcluded = true
			if ns.Watched {
				t.Error("reporting is excluded and reported as watched")
			}
		}
	}
	if !sawExcluded {
		t.Error("an excluded schema vanished from the list, so it could never " +
			"be watched again without typing its name")
	}
}

// TestExcludingChangesWhatTheFingerprintIsOf. The fingerprint is of what is
// watched, so a database watching less is at a different schema — and the next
// read must be a full one rather than a cheap comparison against a digest taken
// under the old answer.
func TestExcludingChangesWhatTheFingerprintIsOf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, twoSchemas())
	scope := st.ForProject(projectID)

	pool.Exec(ctx, `UPDATE schemaver.database SET probe_digest = 'stale' WHERE id = $1`, databaseID)
	if err := scope.WatchNamespaces(ctx, userID, databaseID, []string{"reporting"}); err != nil {
		t.Fatalf("WatchNamespaces: %v", err)
	}
	var probe *string
	pool.QueryRow(ctx, `SELECT probe_digest FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&probe)
	if probe != nil {
		t.Error("the cheap-read digest survived a change to what is watched, so " +
			"the next read would compare against an answer to a different question")
	}

	// And the model itself really drops it.
	whole := twoSchemas()
	if got := len(whole.Without([]string{"reporting"}).Namespaces); got != 1 {
		t.Errorf("Without left %d namespaces, want 1", got)
	}
	a, _ := schema.Fingerprint(whole)
	b, _ := schema.Fingerprint(whole.Without([]string{"reporting"}))
	if a == b {
		t.Error("dropping a schema did not change the fingerprint; the " +
			"fingerprint is supposed to be of what is watched")
	}
}
