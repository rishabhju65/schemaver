package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// table builds a one-table schema with the named text columns, so a fixture
// reads as the thing it is testing rather than as a wall of struct literals.
func table(columns ...schema.Column) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name:   "public",
		Tables: []schema.Table{{Name: "orders", Columns: columns}},
	}}}
}

func text(name string) schema.Column {
	return schema.Column{Name: name, Type: "text", Nullable: true}
}

// threeWayFixture stands up two databases that have both moved on from a shared
// starting point, and returns the project, the author, and the two databases.
//
// The schemas are built here rather than borrowed from whatever the deployment
// happens to have observed, because a merge test needs to control all three
// corners: what they agreed on, what each did next, and therefore what is
// genuinely one side's own work.
func threeWayFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	base, ours, theirs *schema.Schema) (projectID, userID, target, source int64) {
	t.Helper()

	var credentialID int64
	if err := pool.QueryRow(ctx, `
		SELECT m.project_id, m.user_id, i.credential_id
		  FROM schemaver.project_member m
		  JOIN schemaver.instance i ON i.project_id = m.project_id
		 WHERE m.role = 'admin' ORDER BY m.project_id LIMIT 1`).
		Scan(&projectID, &userID, &credentialID); err != nil {
		t.Skipf("no project with an administrator and a server: %v", err)
	}

	// Named for the test that asked for it. A shared host name would collide on
	// the one-endpoint-per-project constraint the moment two of these run in
	// the same deployment.
	host := strings.ToLower(t.Name()) + ".probe.invalid"
	var instanceID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		projectID, "merge-probe-"+t.Name(), host, credentialID).Scan(&instanceID); err != nil {
		// Fatal, not skipped. A fixture that cannot be built is a broken test,
		// and skipping one reports success for a merge that never ran.
		t.Fatalf("create the probe server: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.change_request
		                WHERE database_id IN (SELECT id FROM schemaver.database
		                                       WHERE instance_id = $1)`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
	})

	blob := func(s *schema.Schema) schema.Version {
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
		return fingerprint
	}

	baseFP, oursFP, theirsFP := blob(base), blob(ours), blob(theirs)

	// Each database is created at the shared schema and then observed at its
	// own, which is what a merge base is recovered from: the history, not the
	// current state.
	for _, d := range []struct {
		name string
		now  schema.Version
		into *int64
	}{
		{"merge_target", oursFP, &target},
		{"merge_source", theirsFP, &source},
	} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
			VALUES ($1, $2, true, $3) RETURNING id`,
			instanceID, d.name, string(d.now)).Scan(d.into); err != nil {
			t.Fatalf("create %s: %v", d.name, err)
		}
		// Observed at the base first, then where it is now. The timestamps are
		// explicit so "the last schema both were at" is a fact of the fixture
		// rather than of how fast the test ran.
		if _, err := pool.Exec(ctx, `
			INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms, observed_at)
			VALUES ($1, $2, 1, now() - interval '2 hours'),
			       ($1, $3, 1, now() - interval '1 hour')`,
			*d.into, string(baseFP), string(d.now)); err != nil {
			t.Fatalf("record snapshots for %s: %v", d.name, err)
		}
	}
	return projectID, userID, target, source
}

func mergeTestPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the merge tests")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestMergeKeepsTheTargetsOwnWork is the regression this whole feature exists
// for: a column production grew on its own must not come out as a DROP because
// staging never had it.
func TestMergeKeepsTheTargetsOwnWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, target, source := threeWayFixture(ctx, t, pool,
		table(text("id")),
		table(text("id"), text("audit_ref")), // ours
		table(text("id"), text("channel")),   // theirs
	)
	scope := st.ForProject(projectID)

	view, err := scope.PlanMerge(ctx, target, source)
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	if !view.Diverged() {
		t.Fatal("both sides moved, so this is a divergence")
	}
	if !view.Clean() {
		t.Fatalf("different columns are not a conflict: %v", view.Conflicts)
	}
	if len(view.Apply) != 1 || !strings.Contains(view.Apply[0].Summary, "channel") {
		t.Fatalf("merge should apply only the other side's work, got %v", view.Apply)
	}

	requestID, err := scope.Propose(ctx, userID, target, source, "merge", "")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT sql FROM schemaver.migration_step WHERE migration_id = $1 ORDER BY ordinal`,
		migrationID)
	if err != nil {
		t.Fatalf("read statements: %v", err)
	}
	defer rows.Close()
	var statements []string
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			t.Fatalf("scan: %v", err)
		}
		statements = append(statements, sql)
	}
	for _, sql := range statements {
		if strings.Contains(sql, "audit_ref") {
			t.Errorf("the target's own column is being discarded: %s", sql)
		}
	}
	if len(statements) != 1 || !strings.Contains(statements[0], "channel") {
		t.Errorf("expected only the source's column to be added, got %v", statements)
	}
}

// TestMergeRefusesToPickASide checks that disagreement stops the generation and
// says so on the request, rather than quietly producing one side's version.
func TestMergeRefusesToPickASide(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, target, source := threeWayFixture(ctx, t, pool,
		table(text("id")),
		table(text("id"), schema.Column{Name: "note", Type: "character varying(50)", Nullable: true}),
		table(text("id"), text("note")),
	)
	scope := st.ForProject(projectID)

	view, err := scope.PlanMerge(ctx, target, source)
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	if view.Clean() {
		t.Fatal("both sides gave the same column a different type; that is a conflict")
	}

	requestID, err := scope.Propose(ctx, userID, target, source, "conflict", "")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, err := scope.GenerateMigration(ctx, userID, requestID); err == nil {
		t.Fatal("generated a migration across a conflict")
	}

	// The reason has to reach the page. Dropping it is how a conflict used to
	// read as "the two schemas already agree".
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(state_reason, '') FROM schemaver.change_request WHERE id = $1`,
		requestID).Scan(&reason); err != nil {
		t.Fatalf("read the reason: %v", err)
	}
	if !strings.Contains(reason, "note") {
		t.Errorf("the reason should name the object in dispute, got %q", reason)
	}
}

// TestMergeBaseIsTheLastTimeBothWereThere guards the ordering: the base is the
// most recent schema *both* have been observed at, not the most recent either
// one has.
func TestMergeBaseIsTheLastTimeBothWereThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	base := table(text("id"))
	projectID, _, target, source := threeWayFixture(ctx, t, pool,
		base, table(text("id"), text("audit_ref")), table(text("id"), text("channel")))
	scope := st.ForProject(projectID)

	want, err := schema.Fingerprint(base)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	got, err := scope.MergeBase(ctx, target, source)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != want {
		t.Errorf("merge base = %s, want %s", got.Short(), want.Short())
	}
}

// TestAMergedSchemaIsNotReadableByAnotherTenant guards the reason a merged
// schema is readable at all.
//
// Every other schema is reachable because this project observed a database at
// it. A merged schema is computed, so no snapshot will ever name it, and the
// project that generated it could not otherwise read back the target its own
// migration declares. Widening the rule to "or a migration of this project's
// names it" is what makes that work — and widening a scoping rule is exactly
// where one tenant starts being able to read another's.
func TestAMergedSchemaIsNotReadableByAnotherTenant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, target, source := threeWayFixture(ctx, t, pool,
		table(text("id")),
		table(text("id"), text("audit_ref")),
		table(text("id"), text("channel")),
	)
	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source, "merge", "")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}

	var mergedFP string
	if err := pool.QueryRow(ctx,
		`SELECT to_fingerprint FROM schemaver.migration WHERE id = $1`, migrationID).
		Scan(&mergedFP); err != nil {
		t.Fatalf("read the declared target: %v", err)
	}

	// The project that generated it can read it. Without this the gate and the
	// review page have no way to see their own migration's target.
	if _, err := scope.Blob(ctx, schema.Version(mergedFP)); err != nil {
		t.Fatalf("the generating project cannot read its own merged schema: %v", err)
	}

	// A project that has nothing to do with it cannot, even holding the exact
	// fingerprint — which is the position an attacker who saw a URL is in.
	var otherOrg, otherProject int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO schemaver.organization (name) VALUES ('merge-iso') RETURNING id`).
		Scan(&otherOrg); err != nil {
		t.Fatalf("create the other org: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.project WHERE organization_id = $1`, otherOrg)
		pool.Exec(bg, `DELETE FROM schemaver.organization WHERE id = $1`, otherOrg)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.project (name, organization_id)
		VALUES ('merge-iso', $1) RETURNING id`, otherOrg).Scan(&otherProject); err != nil {
		t.Fatalf("create the other project: %v", err)
	}

	if _, err := st.ForProject(otherProject).Blob(ctx, schema.Version(mergedFP)); err == nil {
		t.Error("another tenant read a merged schema it has no claim to")
	}
}

// TestAMergeSatisfiesThePromotionGate covers the collision merging creates with
// the rule that a change reaches production through the environment below it.
//
// That rule is checked by asking whether the lower environment is already at
// the schema this migration targets. A merge targets neither side's schema —
// it targets the ancestor with both sides' work applied — so the fingerprints
// never match, and on that test alone no merge could ever be executed.
func TestAMergeSatisfiesThePromotionGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, target, source := threeWayFixture(ctx, t, pool,
		table(text("id")),
		table(text("id"), text("audit_ref")),
		table(text("id"), text("channel")),
	)
	// The source is the environment production follows, which is the ordinary
	// arrangement: the change is being promoted up from staging.
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		target, source); err != nil {
		t.Fatalf("set the promotion peer: %v", err)
	}

	scope := st.ForProject(projectID)
	requestID, err := scope.Propose(ctx, userID, target, source, "merge", "")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.PromotionSource != "merge_source" {
		t.Fatalf("promotion source is %q, want merge_source", state.PromotionSource)
	}
	if !state.PromotionReached {
		t.Errorf("the work in this merge came from the lower environment, so it "+
			"has been through it: %s", state.Reason)
	}

	// And the check still expires. Move the lower environment off the schema
	// the work is on, and the gate must shut again.
	if _, err := pool.Exec(ctx, `
		UPDATE schemaver.database SET current_fingerprint =
		    (SELECT from_fingerprint FROM schemaver.migration WHERE id = $2)
		 WHERE id = $1`, source, migrationID); err != nil {
		t.Fatalf("move the lower environment: %v", err)
	}
	moved, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after moving: %v", err)
	}
	if moved.PromotionReached {
		t.Error("the lower environment no longer has this work and the gate stayed open")
	}
}
