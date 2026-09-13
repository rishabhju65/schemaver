package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAuthoredDataStatementSurvivesGeneration checks that a statement no diff
// could ever derive can be carried by a migration.
//
// A backfill is the case: `UPDATE orders SET channel = 'web' WHERE channel IS
// NULL` leaves the schema identical, so no comparison of two schemas will ever
// produce it, and a column cannot be added, filled and made NOT NULL without
// one. Editing a step to hold several statements is how it gets in, since there
// is no way to add a step.
//
// Worth pinning because it works by consequence rather than by design: steps
// hold text, and statements go to the engine through the simple protocol, which
// accepts a batch. Anything that moved execution to the extended protocol — a
// reasonable-looking change, since it is what parameterised queries need —
// would take data migrations away without failing a single existing test.
func TestAuthoredDataStatementSurvivesGeneration(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the authoring test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, userID, target, source int64
	if err := pool.QueryRow(ctx, `
		-- Scoped to one project, chosen as the lowest id holding both fixtures.
		-- Database names are unique per instance, not globally, so an unscoped
		-- lookup returns several rows the moment a second server is registered
		-- with a database of the same name — which is ordinary, and which broke
		-- this query the first time somebody added one.
		WITH fixture AS (
			SELECT i.project_id,
			       min(d.id) FILTER (WHERE d.name = 'shop_prod')    AS target,
			       min(d.id) FILTER (WHERE d.name = 'shop_staging') AS source
			  FROM schemaver.database d
			  JOIN schemaver.instance i ON i.id = d.instance_id
			 WHERE d.name IN ('shop_prod', 'shop_staging')
			   -- Observed, not merely present. These tests diff two schemas, so
			   -- a project whose fixtures have never been read is no use to
			   -- them — and picking one produces "has not been read yet" rather
			   -- than anything about what is under test.
			   AND d.current_fingerprint IS NOT NULL
			 GROUP BY i.project_id
			HAVING count(*) FILTER (WHERE d.name = 'shop_prod') > 0
			   AND count(*) FILTER (WHERE d.name = 'shop_staging') > 0
			 ORDER BY i.project_id
			 LIMIT 1
		)
		SELECT f.project_id,
		       (SELECT m.user_id FROM schemaver.project_member m
		         WHERE m.project_id = f.project_id AND m.role = 'admin' LIMIT 1),
		       f.target, f.source
		  FROM fixture f`).
		Scan(&projectID, &userID, &target, &source); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source, "authoring probe", "")
	if err != nil {
		t.Skipf("nothing to migrate: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.change_request WHERE id = $1`, requestID)
	})
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Skipf("nothing to generate: %v", err)
	}

	detail, err := scope.Request(ctx, requestID)
	if err != nil || len(detail.Steps) == 0 {
		t.Fatalf("no statements to author into: %v", err)
	}
	backfill := detail.Steps[0].SQL +
		"\nUPDATE public.orders SET channel = 'web' WHERE channel IS NULL;"
	if err := scope.EditStatement(ctx, userID, migrationID, false,
		detail.Steps[0].Ordinal, backfill); err != nil {
		t.Fatalf("authoring a data statement into a step: %v", err)
	}

	// It reaches the prover intact, batch and all. That is what the executor
	// will be handed.
	task, err := st.LoadProofTask(ctx, migrationID)
	if err != nil {
		t.Fatalf("LoadProofTask: %v", err)
	}
	if !strings.Contains(task.Statements[0], "UPDATE public.orders") {
		t.Errorf("the authored statement did not survive into the plan:\n%s",
			task.Statements[0])
	}
	if !strings.Contains(task.Statements[0], "ADD COLUMN") {
		t.Errorf("the generated statement was lost when the data one was added:\n%s",
			task.Statements[0])
	}
}
