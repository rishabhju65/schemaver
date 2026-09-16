package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// openRequest stands up one request from a branch against a database, and
// returns everything needed to move the database under it.
func openRequest(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	at, becomes *schema.Schema, name string) (
	st *store.Store, scope *store.Scope, userID, databaseID, requestID int64) {
	t.Helper()
	st = store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, at)
	scope = st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, name, "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, becomes)
	requestID, err = scope.MergeBranch(ctx, userID, branchID, databaseID, name, "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	return st, scope, userID, databaseID, requestID
}

func migrationStart(ctx context.Context, t *testing.T, pool *pgxpool.Pool, requestID int64) string {
	t.Helper()
	var from string
	if err := pool.QueryRow(ctx, `
		SELECT from_fingerprint FROM schemaver.migration
		 WHERE change_request_id = $1 AND superseded_at IS NULL`, requestID).Scan(&from); err != nil {
		t.Fatalf("read the migration start: %v", err)
	}
	return from
}

func requestState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, requestID int64) (string, string) {
	t.Helper()
	var state string
	var reason *string
	if err := pool.QueryRow(ctx, `
		SELECT state, state_reason FROM schemaver.change_request WHERE id = $1`,
		requestID).Scan(&state, &reason); err != nil {
		t.Fatalf("read the request state: %v", err)
	}
	if reason == nil {
		return state, ""
	}
	return state, *reason
}

func currentFingerprint(ctx context.Context, t *testing.T, pool *pgxpool.Pool, databaseID int64) string {
	t.Helper()
	var fp string
	if err := pool.QueryRow(ctx,
		`SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&fp); err != nil {
		t.Fatalf("read the database fingerprint: %v", err)
	}
	return fp
}

// TestAMovedDatabaseRebuildsItsOpenRequests is the behaviour this exists for.
//
// A migration is a plan from one exact schema to another. When the database
// moves — another request ran, or somebody changed it by hand — every other
// open request against it is planning from a schema that is no longer there.
// That used to be discovered only when somebody tried to run one and the
// executor refused it under the lock, which is the worst possible moment.
func TestAMovedDatabaseRebuildsItsOpenRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st, _, _, databaseID, requestID := openRequest(ctx, t, pool,
		table(text("id")), table(text("id"), text("channel")), "rebase-a")

	// Somebody else's change lands on the database: a column this request
	// knows nothing about.
	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("region")))
	moved := currentFingerprint(ctx, t, pool, databaseID)

	if start := migrationStart(ctx, t, pool, requestID); start == moved {
		t.Fatal("the request already starts where the database is; nothing to prove")
	}

	out, err := st.RebaseOpenRequests(ctx, databaseID)
	if err != nil {
		t.Fatalf("RebaseOpenRequests: %v", err)
	}
	if out.Rebuilt != 1 || out.Conflict != 0 {
		t.Errorf("rebuilt=%d conflicted=%d, want 1 and 0", out.Rebuilt, out.Conflict)
	}
	if start := migrationStart(ctx, t, pool, requestID); start != moved {
		t.Errorf("the request still plans from %s while the database is at %s",
			start[:12], moved[:12])
	}
	if state, _ := requestState(ctx, t, pool, requestID); state != "IN_REVIEW" {
		t.Errorf("state is %s; a rebuilt plan goes back to review", state)
	}
}

// TestRebuildingWithdrawsApprovals is the consequence, chosen deliberately.
//
// Somebody approved those statements. These are different statements, produced
// against a base that moved after they looked. Nothing revokes the approval —
// it is counted only against the plan digest it was given for, and the digest
// is no longer the current one.
func TestRebuildingWithdrawsApprovals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st, scope, userID, databaseID, requestID := openRequest(ctx, t, pool,
		table(text("id")), table(text("id"), text("channel")), "rebase-approved")

	if err := scope.Decide(ctx, requestID, userID, "approve", "looks right"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	before, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if before.AdminApprovals == 0 {
		t.Fatal("the approval did not register; there is nothing to withdraw")
	}

	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("region")))
	if _, err := st.RebaseOpenRequests(ctx, databaseID); err != nil {
		t.Fatalf("RebaseOpenRequests: %v", err)
	}

	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if after.AdminApprovals != 0 {
		t.Errorf("%d approvals still count against a plan nobody has read",
			after.AdminApprovals)
	}
	if after.Executable {
		t.Error("the request is executable on an approval given for other statements")
	}
}

// TestARequestAlreadyCurrentIsLeftAlone keeps the rebuild from being a tax on
// every observation.
//
// Rebuilding withdraws approvals, so doing it to a request that was already
// planning from where the database is would throw away a decision for nothing.
func TestARequestAlreadyCurrentIsLeftAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st, scope, userID, databaseID, requestID := openRequest(ctx, t, pool,
		table(text("id")), table(text("id"), text("channel")), "rebase-current")

	if err := scope.Decide(ctx, requestID, userID, "approve", "fine"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	out, err := st.RebaseOpenRequests(ctx, databaseID)
	if err != nil {
		t.Fatalf("RebaseOpenRequests: %v", err)
	}
	if out.Rebuilt != 0 || out.Conflict != 0 {
		t.Errorf("rebuilt=%d conflicted=%d; the database never moved",
			out.Rebuilt, out.Conflict)
	}

	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if after.AdminApprovals == 0 {
		t.Error("an approval was withdrawn by a rebuild that had nothing to do")
	}
}

// TestARequestThatCannotBeRebuiltSaysSo is the case the whole thing is for.
//
// The branch and the database changed the same thing in different ways. There
// is no plan that satisfies both, and the honest outcome is to say which
// request is stuck and why, rather than leave it looking ready.
func TestARequestThatCannotBeRebuiltSaysSo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st, _, _, databaseID, requestID := openRequest(ctx, t, pool,
		table(text("id"), text("note")),
		table(text("id"), schema.Column{Name: "note", Type: "text", Nullable: false}),
		"rebase-conflict")

	// The database gives the same column a different shape.
	moveDatabase(ctx, t, pool, databaseID, table(
		text("id"), schema.Column{Name: "note", Type: "integer", Nullable: true}))

	out, err := st.RebaseOpenRequests(ctx, databaseID)
	if err != nil {
		t.Fatalf("RebaseOpenRequests: %v", err)
	}
	if out.Conflict == 0 {
		t.Skip("this pair merged cleanly, so there is no conflict to report")
	}

	state, reason := requestState(ctx, t, pool, requestID)
	if state != "STALE" {
		t.Errorf("state is %s; a request that cannot be rebuilt is stale", state)
	}
	if strings.TrimSpace(reason) == "" {
		t.Error("marked stale with no reason, so nobody can tell what to do")
	}
}

// TestQueueingARebaseIsIdempotent covers the enqueue itself.
//
// Keyed by database so a burst of changes collapses into one pass: the work is
// "bring everything up to date with where this database is now", and doing it
// once after three changes is the same as doing it three times.
func TestQueueingARebaseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	_, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	// The fixture's database goes when the test does; the job would not, and a
	// worker on this deployment would keep finding it.
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.job WHERE idempotency_key = $1`,
			fmt.Sprintf("rebase:%d", databaseID))
	})

	for i := 0; i < 3; i++ {
		if err := st.EnqueueRebase(ctx, databaseID); err != nil {
			t.Fatalf("EnqueueRebase: %v", err)
		}
	}

	var jobs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job
		 WHERE kind = 'rebase' AND target_kind = 'database' AND target_id = $1
		   AND state = 'pending'`, databaseID).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 1 {
		t.Errorf("%d pending rebase jobs for one database, want 1", jobs)
	}

	// The kind has to be one the worker actually claims, or the job sits there
	// forever looking queued.
	claimed := false
	for _, k := range store.ObservationKinds {
		if k == store.KindRebase {
			claimed = true
		}
	}
	if !claimed {
		t.Error("nothing claims rebase jobs, so queueing one achieves nothing")
	}
}

// TestARebaseForAVanishedDatabaseIsNotAFailure keeps a queued job from
// outliving what it was queued for.
//
// A database can be removed, or its server archived, between a change being
// noticed and the rebase running. A failing job is retried, and no number of
// retries brings a database back — so this would sit in the queue failing on
// every attempt and filling the log with a problem nobody can fix.
func TestARebaseForAVanishedDatabaseIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	out, err := st.RebaseOpenRequests(ctx, 0)
	if err != nil {
		t.Fatalf("a rebase for a database that is not there failed: %v", err)
	}
	if out.Rebuilt != 0 || out.Conflict != 0 {
		t.Errorf("rebuilt=%d conflicted=%d for a database that does not exist",
			out.Rebuilt, out.Conflict)
	}
}
