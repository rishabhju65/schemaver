package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestProposeReviewApprove walks the whole review flow against a live metadata
// database: propose bringing one database in line with another, generate the
// migration, and confirm the execute gate opens only once a project
// administrator has approved.
//
// It needs a deployment that already has a project with two observed databases,
// which is why it is gated on an explicit variable rather than run by default.
func TestProposeReviewApprove(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the review flow test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup rather than defer, and the difference is the whole reason this
	// test kept leaving a fake server behind. Deferred calls run as the test
	// function returns; cleanup callbacks run after that. Closing the pool with
	// defer therefore closed it *before* the cleanup below, so every statement
	// in it failed against a closed pool — silently, because their errors are
	// discarded. Registered here first, it runs last.
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	// Two databases of this test's own, at two schemas that genuinely differ.
	//
	// It used to diff the deployment's own shop_prod against shop_staging, and
	// so depended on them being out of line — which they are only until
	// something brings them together. Every successful migration, including the
	// ones this suite runs, consumed the very drift the next run needed, so it
	// passed once and then reported "the target database already matches the
	// source schema" until somebody re-introduced a difference by hand.
	//
	// No real PostgreSQL databases are involved: propose and generate work from
	// stored schemas, so two rows pointing at two blobs are a complete fixture.
	var projectID, userID, credentialID int64
	if err := pool.QueryRow(ctx, `
		SELECT m.project_id, m.user_id, i.credential_id
		  FROM schemaver.project_member m
		  JOIN schemaver.instance i ON i.project_id = m.project_id
		 WHERE m.role = 'admin' ORDER BY m.project_id LIMIT 1`).
		Scan(&projectID, &userID, &credentialID); err != nil {
		t.Skipf("no project with an administrator and a server: %v", err)
	}

	// Two schemas of this test's own rather than two the deployment happens to
	// hold.
	//
	// It used to take min and max of every fingerprint the project had ever
	// observed, which made the test's subject whatever those two schemas
	// happened to differ by. Adding any unrelated schema to the project changed
	// the pair, and eventually picked one whose difference reads as a rename —
	// at which point the gate stayed shut on an unanswered rename question and
	// a test about approval failed for reasons that had nothing to do with it.
	//
	// Built here instead, so the difference is exactly one added column: no
	// drop, so nothing to mistake for a rename, and a revert that is genuinely
	// the one written below.
	//
	// A blob is readable only through something of the caller's own that names
	// it — that is how one project is kept from reading another's schemas — so
	// the snapshots recorded further down are what make these legible, not the
	// insert.
	before, after := table(text("id")), table(text("id"), text("channel"))
	older, newer := storeSchema(ctx, t, pool, before), storeSchema(ctx, t, pool, after)

	// Named and addressed for the test that asked for it. A shared name or
	// host collides with anything a previous run left behind, and the two
	// constraints — one endpoint per project, one name per project — each
	// produce that collision on their own.
	var instanceID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		projectID, "review-probe-"+t.Name(),
		strings.ToLower(t.Name())+".probe.invalid", credentialID).Scan(&instanceID); err != nil {
		// Fatal, not skipped. A fixture that cannot be built is a broken test,
		// and skipping one reports success for a review flow that never ran.
		t.Fatalf("create the probe server: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// Change requests first. Deleting the databases while one still refers
		// to them fails on the foreign key, and because the error was discarded
		// the whole fixture survived — a fake server, visible in the fleet,
		// holding the host name the next run needs.
		pool.Exec(bg, `DELETE FROM schemaver.change_request
		                WHERE database_id IN (SELECT id FROM schemaver.database
		                                       WHERE instance_id = $1)`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
	})

	var target, source int64
	for _, d := range []struct {
		name        string
		fingerprint string
		into        *int64
	}{
		{"probe_target", older, &target},
		{"probe_source", newer, &source},
	} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
			VALUES ($1, $2, true, $3) RETURNING id`,
			instanceID, d.name, d.fingerprint).Scan(d.into); err != nil {
			t.Fatalf("create %s: %v", d.name, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms)
			VALUES ($1, $2, 1)`, *d.into, d.fingerprint); err != nil {
			t.Fatalf("record the snapshot for %s: %v", d.name, err)
		}
	}

	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source,
		"bring production in line with staging", "generated by the review flow test")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Logf("change request %d", requestID)

	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	t.Logf("migration %d", migrationID)

	var steps int
	var irreversible *string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM schemaver.migration_step WHERE migration_id = $1),
		       (SELECT irreversible_reason FROM schemaver.migration WHERE id = $1)`,
		migrationID).Scan(&steps, &irreversible); err != nil {
		t.Fatalf("inspect migration: %v", err)
	}
	if steps == 0 {
		t.Fatal("no statements generated")
	}
	t.Logf("%d statements; irreversible: %v", steps, irreversible != nil)

	rows, _ := pool.Query(ctx,
		`SELECT ordinal, sql, transactional FROM schemaver.migration_step
		  WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	for rows.Next() {
		var n int
		var sql string
		var tx bool
		_ = rows.Scan(&n, &sql, &tx)
		mark := " "
		if !tx {
			mark = "!"
		}
		t.Logf("  %s %d. %s", mark, n, sql)
	}
	rows.Close()

	// A freshly generated migration is shut on its proof before anybody has
	// even looked at it: approving something that has not been shown to produce
	// the schema it claims would be approving a guess.
	// A freshly generated migration has no way back, because nothing generates
	// one: whoever wrote the change writes the revert (D-022). The gate says so
	// before it says anything about proofs, since this is the author's to fix
	// and a proof cannot run on a plan that is not finished.
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.Executable {
		t.Error("executable with no way back written")
	}
	t.Logf("gate closed on the missing revert: %s", state.Reason)

	// Stand in for the worker, which is not running here. Both halves: the gate
	// wants the migration rehearsed and the way back shown to lead back.
	if err := st.RecordProof(ctx, migrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}

	// Before approval the gate must still be shut, and it must say why.
	state, err = scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.Executable {
		t.Error("executable with no approval")
	}
	t.Logf("gate closed: %s", state.Reason)

	// A non-administrator cannot open it.
	var viewer int64
	err = pool.QueryRow(ctx, `
		SELECT user_id FROM schemaver.project_member
		 WHERE project_id = $1 AND role <> 'admin' LIMIT 1`, projectID).Scan(&viewer)
	if err == nil {
		if derr := scope.Decide(ctx, requestID, viewer, "approve", ""); !errors.Is(derr, store.ErrNotAdmin) {
			t.Errorf("a non-administrator approval was not refused: %v", derr)
		}
	}

	// The author is the project's only administrator here, so their own
	// approval is the only one obtainable and is recorded as self-approved.
	if err := scope.Decide(ctx, requestID, userID, "approve", "sole administrator"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	state, err = scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if !state.Executable {
		t.Errorf("still not executable after an administrator approved: %s", state.Reason)
	}
	if len(state.Decisions) != 1 || !state.Decisions[0].SelfApproved {
		t.Errorf("self-approval not recorded: %+v", state.Decisions)
	}
	t.Logf("gate open; %d administrator approval(s), self-approved=%v",
		state.AdminApprovals, state.Decisions[0].SelfApproved)

	// Regenerating supersedes the migration, and both the approval and the proof
	// stop applying without anything having to withdraw them. Both are evidence
	// about one fingerprint pair, and the new migration is a different row.
	if _, err := scope.GenerateMigration(ctx, userID, requestID); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	state, err = scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after regeneration: %v", err)
	}
	if state.Executable {
		t.Error("the approval survived regeneration; evidence must expire with the migration")
	}
	t.Logf("after regeneration the gate is shut again: %s", state.Reason)
}
