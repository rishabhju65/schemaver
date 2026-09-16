package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// landed does to a request what executing it does: the database arrives at the
// migration's target and the request is finished with.
//
// Worth its own helper because forgetting the second half makes a test lie. A
// request left open after its own change landed is rebased against the database
// it just moved, and reports a conflict that would never happen.
func landed(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	databaseID, requestID int64, to *schema.Schema) {
	t.Helper()
	moveDatabase(ctx, t, pool, databaseID, to)
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.change_request SET state = 'COMPLETED' WHERE id = $1`,
		requestID); err != nil {
		t.Fatalf("complete request %d: %v", requestID, err)
	}
}

// TestParallelBranchesOnOneDatabase is the everyday case: two people branch
// from staging at the same point and one of them lands first.
//
// The second request must end up planning from where staging now is, and must
// still contain only its own author's work. Carrying the first author's change
// into it would be a plan that fails on a column that already exists.
func TestParallelBranchesOnOneDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, staging := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	first, err := scope.CutBranch(ctx, userID, staging, "parallel-first", "")
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	second, err := scope.CutBranch(ctx, userID, staging, "parallel-second", "")
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	advance(ctx, t, pool, first, table(text("id"), text("first_col")))
	advance(ctx, t, pool, second, table(text("id"), text("second_col")))

	// The second author opens their request first, so it is already in review
	// when the other lands.
	secondReq, err := scope.MergeBranch(ctx, userID, second, staging, "second", "")
	if err != nil {
		t.Fatalf("merge second: %v", err)
	}
	firstReq, err := scope.MergeBranch(ctx, userID, first, staging, "first", "")
	if err != nil {
		t.Fatalf("merge first: %v", err)
	}
	landed(ctx, t, pool, staging, firstReq, table(text("id"), text("first_col")))

	out, err := st.RebaseOpenRequests(ctx, staging)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if out.Rebuilt != 1 || out.Conflict != 0 {
		t.Errorf("rebuilt=%d conflicted=%d, want 1 and 0 — the two changed "+
			"different columns and have nothing to disagree about",
			out.Rebuilt, out.Conflict)
	}

	now := currentFingerprint(ctx, t, pool, staging)
	if start := migrationStart(ctx, t, pool, secondReq); start != now {
		t.Errorf("the open request still plans from %s while staging is at %s",
			start[:12], now[:12])
	}
	if state, _ := requestState(ctx, t, pool, secondReq); state != "IN_REVIEW" {
		t.Errorf("state is %s, want IN_REVIEW after a rebuild", state)
	}

	got := statementsOf(ctx, t, pool, secondReq)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "second_col") {
		t.Errorf("the author's own change is missing: %v", got)
	}
	if strings.Contains(joined, "first_col") {
		t.Errorf("the other author's change was pulled in, so this plan adds a "+
			"column that is already there: %v", got)
	}
}

// TestParallelBranchesThatCollide is the case a person has to settle.
//
// Both added the same column with different types. There is no plan that
// satisfies both, and the request must say so rather than sit in the list
// looking ready.
func TestParallelBranchesThatCollide(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, staging := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	first, _ := scope.CutBranch(ctx, userID, staging, "collide-first", "")
	second, _ := scope.CutBranch(ctx, userID, staging, "collide-second", "")
	advance(ctx, t, pool, first, table(text("id"),
		schema.Column{Name: "shared", Type: "text", Nullable: true}))
	advance(ctx, t, pool, second, table(text("id"),
		schema.Column{Name: "shared", Type: "integer", Nullable: true}))

	secondReq, err := scope.MergeBranch(ctx, userID, second, staging, "second", "")
	if err != nil {
		t.Fatalf("merge second: %v", err)
	}
	firstReq, err := scope.MergeBranch(ctx, userID, first, staging, "first", "")
	if err != nil {
		t.Fatalf("merge first: %v", err)
	}
	landed(ctx, t, pool, staging, firstReq, table(text("id"),
		schema.Column{Name: "shared", Type: "text", Nullable: true}))

	out, err := st.RebaseOpenRequests(ctx, staging)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if out.Conflict != 1 {
		t.Fatalf("conflicted=%d, want 1", out.Conflict)
	}

	state, reason := requestState(ctx, t, pool, secondReq)
	if state != "STALE" {
		t.Errorf("state is %s, want STALE", state)
	}
	// The reason has to name the object, or somebody has to go and find it.
	if !strings.Contains(reason, "shared") {
		t.Errorf("the reason does not say what conflicts: %q", reason)
	}
}

// TestABranchMergedLongAfterItWasCut covers the branch nobody opened a request
// for.
//
// A rebase only touches requests. A branch that simply sits there while other
// work lands is reconciled when it is finally merged, by the same three-way
// merge — so being slow to open a request costs nothing, and the result is
// recorded as a merge so review and the promotion gate both know the target is
// a schema neither side was at.
func TestABranchMergedLongAfterItWasCut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, staging := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	first, _ := scope.CutBranch(ctx, userID, staging, "late-first", "")
	late, _ := scope.CutBranch(ctx, userID, staging, "late-second", "")
	cutAt := currentFingerprint(ctx, t, pool, staging)
	advance(ctx, t, pool, first, table(text("id"), text("first_col")))
	advance(ctx, t, pool, late, table(text("id"), text("late_col")))

	firstReq, err := scope.MergeBranch(ctx, userID, first, staging, "first", "")
	if err != nil {
		t.Fatalf("merge first: %v", err)
	}
	landed(ctx, t, pool, staging, firstReq, table(text("id"), text("first_col")))

	lateReq, err := scope.MergeBranch(ctx, userID, late, staging, "late", "")
	if err != nil {
		t.Fatalf("merge late: %v", err)
	}

	now := currentFingerprint(ctx, t, pool, staging)
	if start := migrationStart(ctx, t, pool, lateReq); start != now {
		t.Errorf("plans from %s, but staging is at %s", start[:12], now[:12])
	}
	joined := strings.Join(statementsOf(ctx, t, pool, lateReq), "\n")
	if !strings.Contains(joined, "late_col") || strings.Contains(joined, "first_col") {
		t.Errorf("a late merge should carry only its own work: %q", joined)
	}

	var mergeBase *string
	if err := pool.QueryRow(ctx, `
		SELECT merge_base FROM schemaver.migration
		 WHERE change_request_id = $1 AND superseded_at IS NULL`,
		lateReq).Scan(&mergeBase); err != nil {
		t.Fatalf("read the merge base: %v", err)
	}
	if mergeBase == nil {
		t.Fatal("not recorded as a merge, so the promotion gate will look for " +
			"an environment below that is already at a schema nobody is at")
	}
	if *mergeBase != cutAt {
		t.Errorf("merge base is %s, want the point both branched from, %s",
			(*mergeBase)[:12], cutAt[:12])
	}
}
