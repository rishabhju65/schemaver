package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestPipelineRunsInOrderAndOnlyOnce covers what a pipeline has to get right
// once one request reaches several databases.
//
// The chain comes from the Follows mapping rather than from the form, so the
// order is the promotion order and not whatever happened to be submitted. The
// next target is worked out at each press rather than fixed when the request
// was opened, because a database may have reached the schema another way. And a
// target already there is skipped, since applying the statements twice fails on
// the first column that already exists.
func TestPipelineRunsInOrderAndOnlyOnce(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the pipeline test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, credentialID int64
	if err := pool.QueryRow(ctx, `
		SELECT p.id, i.credential_id FROM schemaver.project p
		  JOIN schemaver.instance i ON i.project_id = p.id LIMIT 1`).
		Scan(&projectID, &credentialID); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	// Three databases of this test's own, chained dev -> stg -> prd.
	var instance int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, 'pipeline-probe', 'probe.invalid', $2) RETURNING id`,
		projectID, credentialID).Scan(&instance); err != nil {
		t.Skipf("create instance: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instance)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instance)
	})

	var start string
	if err := pool.QueryRow(ctx,
		`SELECT min(fingerprint) FROM schemaver.schema_blob`).Scan(&start); err != nil {
		t.Skipf("no schema to start from: %v", err)
	}
	ids := map[string]int64{}
	for _, name := range []string{"pdev", "pstg", "pprd"} {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
			VALUES ($1, $2, true, $3) RETURNING id`, instance, name, start).Scan(&id); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		ids[name] = id
	}
	// pstg follows pdev; pprd follows pstg.
	pool.Exec(ctx, `UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		ids["pstg"], ids["pdev"])
	pool.Exec(ctx, `UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		ids["pprd"], ids["pstg"])

	// The chain from the bottom names all three, in order.
	chain, err := scope.Chain(ctx, ids["pdev"])
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("the chain has %d step(s), want 3: %+v", len(chain), chain)
	}
	for i, want := range []string{"pdev", "pstg", "pprd"} {
		if chain[i].Name != want {
			t.Errorf("step %d is %s, want %s — the order must come from the "+
				"mapping, not from how the form was filled in", i, chain[i].Name, want)
		}
	}

	// And from the middle it names only what is above.
	upper, err := scope.Chain(ctx, ids["pstg"])
	if err != nil {
		t.Fatalf("Chain from the middle: %v", err)
	}
	if len(upper) != 2 || upper[0].Name != "pstg" || upper[1].Name != "pprd" {
		t.Errorf("from pstg the chain is %+v, want pstg then pprd", upper)
	}
}

// TestDiscoveredDatabasesAreTaggedProduction covers the default a database
// arrives with.
//
// The label decides something since D-024: it tells a promotion link pointing
// the right way from one pointing backwards, and an unlabelled database makes
// that check permissive because there are no ranks to compare. So leaving it
// unset — which is what happened unless somebody went and chose one — quietly
// disabled a safeguard in the common case.
//
// Defaulting to the most guarded environment is the only defensible direction.
// Mislabelling staging as production costs ceremony somebody removes;
// mislabelling production as staging lets a change reach it without passing
// through anything.
func TestDiscoveredDatabasesAreTaggedProduction(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the tagging test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, credentialID int64
	if err := pool.QueryRow(ctx, `
		SELECT p.id, i.credential_id FROM schemaver.project p
		  JOIN schemaver.instance i ON i.project_id = p.id LIMIT 1`).
		Scan(&projectID, &credentialID); err != nil {
		t.Skipf("no fixtures: %v", err)
	}

	var instance int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, 'tagging-probe', 'tagging.invalid', $2) RETURNING id`,
		projectID, credentialID).Scan(&instance); err != nil {
		// Fatal rather than skipped: a fixture that will not build is a
		// failure, and skipping on it means this test reports success on every
		// run where it never ran at all.
		t.Fatalf("create instance: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instance)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instance)
	})

	if _, _, err := st.SyncDatabases(ctx, instance, []introspect.Database{
		{Name: "found_one", Owner: "someone", Encoding: "UTF8"},
	}); err != nil {
		t.Fatalf("SyncDatabases: %v", err)
	}

	var tag string
	var rank int
	if err := pool.QueryRow(ctx, `
		SELECT e.name, e.rank FROM schemaver.database d
		  JOIN schemaver.environment e ON e.id = d.environment_id
		 WHERE d.instance_id = $1 AND d.name = 'found_one'`, instance).
		Scan(&tag, &rank); err != nil {
		t.Fatalf("a discovered database carries no tag at all: %v", err)
	}

	// The highest rank in the project, whatever it is called. Rank is what the
	// checks compare, and a project may rename its environments.
	var highest int
	if err := pool.QueryRow(ctx,
		`SELECT max(rank) FROM schemaver.environment WHERE project_id = $1`,
		projectID).Scan(&highest); err != nil {
		t.Fatalf("read the project's ranks: %v", err)
	}
	if rank != highest {
		t.Errorf("discovered as %s (rank %d), want the most guarded environment "+
			"(rank %d): an unknown database should be treated as the dangerous case",
			tag, rank, highest)
	}
}
