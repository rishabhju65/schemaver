package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestABranchCannotOpenTwoRequestsAtOnce is the defect.
//
// Nothing stopped a branch being merged twice into the same database, so
// pressing the button again — or having two tabs open, or going back — opened a
// second request carrying the same statements. Both sat in review looking real
// and both were approvable; the second only became visibly wrong after the
// first one ran.
func TestABranchCannotOpenTwoRequestsAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "dup-probe", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("channel")))

	first, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "first", "")
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}

	_, err = scope.MergeBranch(ctx, userID, branchID, databaseID, "second", "")
	if err == nil {
		t.Fatal("a second request opened for the same branch and database; two " +
			"plans for the same work now sit in review looking equally real")
	}
	// The message has to point at the one that exists, or somebody has to go
	// and find it.
	if !strings.Contains(err.Error(), "already open") {
		t.Errorf("unhelpful refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "#") {
		t.Errorf("the refusal does not name the existing request: %v", err)
	}

	live, err := scope.LiveRequests(ctx, branchID)
	if err != nil {
		t.Fatalf("LiveRequests: %v", err)
	}
	if len(live) != 1 || live[0].ID != first {
		t.Errorf("the branch page would show %v, want just #%d", live, first)
	}
}

// TestABranchMayLandAgainAfterItsRequestIsSettled keeps the guard from becoming
// a one-change-per-branch rule.
//
// A branch that has moved on since it last landed is entitled to land again.
func TestABranchMayLandAgainAfterItsRequestIsSettled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "again-probe", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("one")))

	first, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "first", "")
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	// It ran and somebody is finished with it.
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.change_request SET state = 'DONE' WHERE id = $1`,
		first); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// The branch does more work and lands it.
	advance(ctx, t, pool, branchID, table(text("id"), text("one"), text("two")))
	if _, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "second", ""); err != nil {
		t.Fatalf("a settled request blocked the next one: %v", err)
	}

	live, err := scope.LiveRequests(ctx, branchID)
	if err != nil {
		t.Fatalf("LiveRequests: %v", err)
	}
	if len(live) != 1 {
		t.Errorf("%d live requests, want 1 — the settled one should not count", len(live))
	}
}
