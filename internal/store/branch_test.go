package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// branchFixture stands up a project with one database at a known schema, and
// returns everything a branch test needs.
//
// The database is this test's own and no real PostgreSQL is involved: cutting a
// branch reads a stored schema, and everything after that works from stored
// schemas too. The one operation that needs a live server — applying written
// DDL — is exercised separately, where a shadow is actually configured.
func branchFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	at *schema.Schema) (projectID, userID, databaseID int64) {
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

	var instanceID int64
	host := strings.ToLower(t.Name()) + ".branch.invalid"
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		projectID, "branch-probe-"+t.Name(), host, credentialID).Scan(&instanceID); err != nil {
		t.Fatalf("create the probe server: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.change_request
		                WHERE database_id IN (SELECT id FROM schemaver.database
		                                       WHERE instance_id = $1)`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.branch
		                WHERE origin_database_id IN (SELECT id FROM schemaver.database
		                                              WHERE instance_id = $1)`, instanceID)
		pool.Exec(bg, `UPDATE schemaver.database SET expected_peer_id = NULL
		                WHERE instance_id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
	})

	fingerprint, err := schema.Fingerprint(at)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	canonical, err := schema.Canonical(at)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, 1, $3) ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, len(canonical)); err != nil {
		t.Fatalf("store blob: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
		VALUES ($1, 'branch_origin', true, $2) RETURNING id`,
		instanceID, string(fingerprint)).Scan(&databaseID); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms)
		VALUES ($1, $2, 1)`, databaseID, string(fingerprint)); err != nil {
		t.Fatalf("record the snapshot: %v", err)
	}
	return projectID, userID, databaseID
}

// advance moves a branch to a schema directly, standing in for the worker.
//
// What the worker does is apply somebody's DDL to a throwaway database and
// record the result; what every test below is actually about is the branch
// having moved. Driving that through a live shadow would make these tests
// depend on a server they do not need.
func advance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, branchID int64, to *schema.Schema) {
	t.Helper()
	fingerprint, err := schema.Fingerprint(to)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	canonical, _ := schema.Canonical(to)
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, 1, $3) ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, len(canonical)); err != nil {
		t.Fatalf("store blob: %v", err)
	}
	var from string
	pool.QueryRow(ctx, `SELECT head_fingerprint FROM schemaver.branch WHERE id = $1`,
		branchID).Scan(&from)
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.branch_commit
		    (branch_id, ordinal, from_fingerprint, to_fingerprint, sql)
		VALUES ($1, (SELECT COALESCE(max(ordinal), 0) + 1 FROM schemaver.branch_commit
		              WHERE branch_id = $1), $2, $3, 'written by the test')`,
		branchID, from, string(fingerprint)); err != nil {
		t.Fatalf("record the commit: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.branch SET head_fingerprint = $2 WHERE id = $1`,
		branchID, string(fingerprint)); err != nil {
		t.Fatalf("advance the branch: %v", err)
	}
}

// moveDatabase puts the origin database at a new schema, the way an
// observation would: the blob stored, a snapshot recorded, and the current
// fingerprint moved. Updating the fingerprint alone would leave a database
// pointing at a schema nothing says it has ever been at.
func moveDatabase(ctx context.Context, t *testing.T, pool *pgxpool.Pool, databaseID int64, to *schema.Schema) {
	t.Helper()
	fingerprint, err := schema.Fingerprint(to)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	canonical, _ := schema.Canonical(to)
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, 1, $3) ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, len(canonical)); err != nil {
		t.Fatalf("store blob: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms)
		VALUES ($1, $2, 1)`, databaseID, string(fingerprint)); err != nil {
		t.Fatalf("record the snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET current_fingerprint = $2 WHERE id = $1`,
		databaseID, string(fingerprint)); err != nil {
		t.Fatalf("move the database: %v", err)
	}
}

// TestABranchStartsWhereItWasCut is the property every merge later depends on.
func TestABranchStartsWhereItWasCut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "add-channel", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	b, err := scope.Branch(ctx, id)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if b.Base != b.Head {
		t.Errorf("a branch nobody has written to has diverged by nothing, got %s → %s",
			b.Base.Short(), b.Head.Short())
	}
	if b.Diverged() {
		t.Error("a fresh branch reports as diverged")
	}

	// The origin moving does not move the branch. That is what independent
	// means here, and it is what makes the recorded base an ancestor rather
	// than a moving target.
	moved := table(text("id"), text("unrelated"))
	moveDatabase(ctx, t, pool, databaseID, moved)

	after, err := scope.Branch(ctx, id)
	if err != nil {
		t.Fatalf("Branch after the origin moved: %v", err)
	}
	if after.Base != b.Base {
		t.Errorf("the branch's base followed the database it was cut from: %s → %s",
			b.Base.Short(), after.Base.Short())
	}
}

// TestABranchShowsExactlyWhatDiverged is the question the feature exists to
// answer, and it is asked of the two endpoints rather than of the history.
func TestABranchShowsExactlyWhatDiverged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "diverge", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, id, table(text("id"), text("channel")))
	// Added and then taken away again: the difference is nothing, whatever the
	// journey was.
	advance(ctx, t, pool, id, table(text("id"), text("channel"), text("scratch")))
	advance(ctx, t, pool, id, table(text("id"), text("channel")))

	result, err := scope.BranchDiff(ctx, id)
	if err != nil {
		t.Fatalf("BranchDiff: %v", err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("expected one change, got %v", result.Changes)
	}
	if !strings.Contains(result.Changes[0].Summary, "channel") {
		t.Errorf("the divergence should be the column that survived, got %q",
			result.Changes[0].Summary)
	}

	commits, err := scope.BranchCommits(ctx, id)
	if err != nil {
		t.Fatalf("BranchCommits: %v", err)
	}
	if len(commits) != 3 {
		t.Errorf("the history should keep every write, got %d", len(commits))
	}
}

// TestMergingABranchKeepsTheDatabasesOwnWork is D-027 with an exact ancestor.
func TestMergingABranchKeepsTheDatabasesOwnWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "branch-work", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, id, table(text("id"), text("channel")))

	// Meanwhile the database grew a column of its own.
	ours := table(text("id"), text("audit_ref"))
	moveDatabase(ctx, t, pool, databaseID, ours)

	m, err := scope.PlanBranchMerge(ctx, id, databaseID)
	if err != nil {
		t.Fatalf("PlanBranchMerge: %v", err)
	}
	if !m.Clean() {
		t.Fatalf("different columns are not a conflict: %v", m.Conflicts)
	}
	if len(m.Apply) != 1 || !strings.Contains(m.Apply[0].Summary, "channel") {
		t.Fatalf("the merge should apply only the branch's work, got %v", m.Apply)
	}

	requestID, err := scope.MergeBranch(ctx, userID, id, databaseID, "", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT st.sql, st.change_id FROM schemaver.migration_step st
		  JOIN schemaver.migration m ON m.id = st.migration_id
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL
		 ORDER BY st.ordinal`, requestID)
	if err != nil {
		t.Fatalf("read statements: %v", err)
	}
	defer rows.Close()
	var statements int
	for rows.Next() {
		var sql, changeID string
		if err := rows.Scan(&sql, &changeID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		statements++
		if strings.Contains(sql, "audit_ref") {
			t.Errorf("the database's own column is being discarded: %s", sql)
		}
		// Generated statements name the change they serve. A written one says
		// "authored" because that is all that can honestly be said of it.
		if changeID == "authored" {
			t.Errorf("a merged statement should name its change, got %q on %q",
				changeID, sql)
		}
	}
	if statements != 1 {
		t.Errorf("expected one statement, got %d", statements)
	}

	// The request records where it came from, or the page cannot explain why
	// there is no source database on a comparison.
	var branchID *int64
	pool.QueryRow(ctx, `SELECT branch_id FROM schemaver.change_request WHERE id = $1`,
		requestID).Scan(&branchID)
	if branchID == nil || *branchID != id {
		t.Error("the request does not record the branch it came from")
	}
}

// TestMergingABranchRefusesToPickASide: a disagreement is a question for a
// person, and answering it silently loses the version nobody chose.
func TestMergingABranchRefusesToPickASide(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "conflicting", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, id, table(text("id"), text("note")))

	ours := table(text("id"),
		schema.Column{Name: "note", Type: "character varying(50)", Nullable: true})
	moveDatabase(ctx, t, pool, databaseID, ours)

	if _, err := scope.MergeBranch(ctx, userID, id, databaseID, "", ""); err == nil {
		t.Fatal("merged across a conflict")
	} else if !strings.Contains(err.Error(), "note") {
		t.Errorf("the refusal should name the object in dispute, got %q", err)
	}
}

// TestAClosedBranchTakesNoMoreWork.
func TestAClosedBranchTakesNoMoreWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "short-lived", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	if err := scope.CloseBranch(ctx, userID, id, "done with it"); err != nil {
		t.Fatalf("CloseBranch: %v", err)
	}
	if err := scope.WriteToBranch(ctx, userID, id, "ALTER TABLE x ADD COLUMN y text;", ""); err == nil {
		t.Error("a closed branch accepted a write")
	}

	// And the name is free again, because a branch is a working area rather
	// than a permanent claim on a word.
	if _, err := scope.CutBranch(ctx, userID, databaseID, "short-lived", ""); err != nil {
		t.Errorf("the name of a closed branch is still taken: %v", err)
	}
}

// TestOneBranchNamePerProjectWhileOpen.
func TestOneBranchNamePerProjectWhileOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	if _, err := scope.CutBranch(ctx, userID, databaseID, "taken", ""); err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	_, err := scope.CutBranch(ctx, userID, databaseID, "taken", "")
	if err == nil {
		t.Fatal("two open branches share a name")
	}
	if !strings.Contains(err.Error(), "already open") {
		t.Errorf("the refusal should say why, got %q", err)
	}
}

// TestABranchIsNotVisibleToAnotherTenant.
func TestABranchIsNotVisibleToAnotherTenant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	id, err := st.ForProject(projectID).CutBranch(ctx, userID, databaseID, "private", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}

	var otherOrg, otherProject int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO schemaver.organization (name) VALUES ('branch-iso') RETURNING id`).
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
		VALUES ('branch-iso', $1) RETURNING id`, otherOrg).Scan(&otherProject); err != nil {
		t.Fatalf("create the other project: %v", err)
	}
	other := st.ForProject(otherProject)

	if _, err := other.Branch(ctx, id); !errors.Is(err, store.ErrNoSuchBranch) {
		t.Errorf("another tenant read the branch: %v", err)
	}
	if err := other.WriteToBranch(ctx, userID, id, "ALTER TABLE x ADD COLUMN y text;", ""); !errors.Is(err, store.ErrNoSuchBranch) {
		t.Errorf("another tenant wrote to the branch: %v", err)
	}
	if err := other.CloseBranch(ctx, userID, id, ""); !errors.Is(err, store.ErrNoSuchBranch) {
		t.Errorf("another tenant closed the branch: %v", err)
	}
	branches, err := other.Branches(ctx)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	for _, b := range branches {
		if b.ID == id {
			t.Error("the branch appeared in another tenant's list")
		}
	}
}

// TestABranchMergeRecordsItsBase covers the shortcut the promotion gate takes.
//
// "Has this been through staging" is asked as "is staging already at the schema
// this migration targets". A merge targets the ancestor with both sides' work
// applied, which is a schema staging is never at, so without the base recorded
// the gate would shut permanently on every merge from a branch — and the page
// would explain a plain comparison while showing fewer statements than the two
// schemas differ by.
func TestABranchMergeRecordsItsBase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	id, err := scope.CutBranch(ctx, userID, databaseID, "records-base", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	base, err := scope.Branch(ctx, id)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	advance(ctx, t, pool, id, table(text("id"), text("channel")))

	// Only the branch has moved: an ordinary bring-into-line, and claiming a
	// merge would have the page explaining one that did not happen.
	plain, err := scope.MergeBranch(ctx, userID, id, databaseID, "plain", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	var recorded *string
	if err := pool.QueryRow(ctx, `
		SELECT m.merge_base FROM schemaver.migration m
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL`, plain).
		Scan(&recorded); err != nil {
		t.Fatalf("read the merge base: %v", err)
	}
	if recorded != nil {
		t.Errorf("a change where only the branch moved is not a merge, got base %q", *recorded)
	}

	// Closed before the next one is opened. A branch may have one request in
	// play against a database at a time, and this test wants two in sequence
	// rather than two at once.
	if err := scope.CloseRequest(ctx, userID, plain, "checking the other case"); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}

	// Now the database moves too, and it is.
	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("audit_ref")))
	merged, err := scope.MergeBranch(ctx, userID, id, databaseID, "merged", "")
	if err != nil {
		t.Fatalf("MergeBranch after both moved: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT m.merge_base FROM schemaver.migration m
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL`, merged).
		Scan(&recorded); err != nil {
		t.Fatalf("read the merge base: %v", err)
	}
	if recorded == nil {
		t.Fatal("both sides moved and no merge base was recorded; the promotion " +
			"gate would shut on this forever")
	}
	if *recorded != string(base.Base) {
		t.Errorf("merge base = %s, want the schema the branch was cut from (%s)",
			schema.Version(*recorded).Short(), base.Base.Short())
	}
}

// TestAMergedBranchCanStillReachProduction is the same guarantee seen from the
// gate rather than from the column.
func TestAMergedBranchCanStillReachProduction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	// A lower environment for the database the branch will merge into.
	var instanceID, peerID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
		VALUES ($1, 'branch_peer', true, (SELECT current_fingerprint FROM schemaver.database WHERE id = $2))
		RETURNING id`, instanceID, databaseID).Scan(&peerID); err != nil {
		t.Fatalf("create the lower environment: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		databaseID, peerID); err != nil {
		t.Fatalf("set the promotion peer: %v", err)
	}

	id, err := scope.CutBranch(ctx, userID, databaseID, "reaches-prod", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, id, table(text("id"), text("channel")))
	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("audit_ref")))

	requestID, err := scope.MergeBranch(ctx, userID, id, databaseID, "reaching", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}

	// The lower environment has not had the branch's work, so the gate holds.
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.PromotionReached {
		t.Error("the lower environment does not have this work and the gate opened")
	}

	// Once it does, the gate opens — without anything re-approving, and without
	// the lower environment ever being at the merged schema, which contains the
	// target database's own work and is not something it could reach.
	moveDatabase(ctx, t, pool, peerID, table(text("id"), text("channel")))
	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after the lower environment caught up: %v", err)
	}
	if !after.PromotionReached {
		t.Errorf("the lower environment has this change and the gate stayed shut: %s",
			after.Reason)
	}
}

// tbl and withTables build multi-table fixtures, for tests about which objects
// differ rather than which columns do.
func tbl(name string, cols ...schema.Column) schema.Table {
	return schema.Table{Name: name, Columns: cols}
}

func withTables(tables ...schema.Table) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{Name: "public", Tables: tables}}}
}

// reachable makes stored schemas readable by a project, the way an observation
// would: a scoped read needs something of the caller's own to name the schema.
func reachable(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, fingerprints ...string) {
	t.Helper()
	var databaseID int64
	if err := pool.QueryRow(ctx, `
		SELECT d.id FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE i.project_id = $1 LIMIT 1`, projectID).Scan(&databaseID); err != nil {
		t.Fatalf("no database in the project to attach a snapshot to: %v", err)
	}
	for _, f := range fingerprints {
		if _, err := pool.Exec(ctx, `
			INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms)
			VALUES ($1, $2, 1)`, databaseID, f); err != nil {
			t.Fatalf("make %s reachable: %v", f, err)
		}
	}
}

// schemaVersion is the cast from a stored fingerprint to the typed one.
func schemaVersion(f string) schema.Version { return schema.Version(f) }
