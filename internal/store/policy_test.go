package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// gated stands up a request with everything done except the two things that are
// now policy: nobody has written a way back and nobody has approved.
func gated(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (*store.Scope, int64, int64) {
	t.Helper()
	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	// The policy belongs to the project, and these tests borrow a project that
	// other tests share. Left behind, a loosened policy would quietly open the
	// gate for every test that ran afterwards — including the one asserting
	// that the defaults are what they always were.
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.project_policy WHERE project_id = $1`, projectID)
	})

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "policy-probe", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("channel")))
	requestID, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "policy", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	// The rehearsal is the machine's part and is not policy; stand in for it.
	if err := st.RecordProof(ctx, state.MigrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}
	return scope, userID, requestID
}

// TestTheDefaultPolicyIsWhatEveryProjectAlreadyHad is the guarantee that made
// this safe to add: a project that has never heard of the policy is gated
// exactly as before.
func TestTheDefaultPolicyIsWhatEveryProjectAlreadyHad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, _, requestID := gated(ctx, t, pool)
	p, err := scope.Policy(ctx)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if p.ApprovalsRequired != 1 {
		t.Errorf("defaults changed: %+v", p)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.Executable {
		t.Error("executable with nobody having approved it")
	}
	if !strings.Contains(state.Reason, "approved") {
		t.Errorf("expected the approval gate, got %q", state.Reason)
	}
}

// TestAProjectCanStopAskingForApproval: with both off, a rehearsed change is
// ready the moment it is proven — which is the whole point of the setting.
func TestAProjectCanStopAskingForApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := gated(ctx, t, pool)
	if err := scope.SetPolicy(ctx, userID, store.Policy{
		ApprovalsRequired: 0,
	}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if !state.Executable {
		t.Errorf("a project asking for nothing still refuses: %s", state.Reason)
	}
}

// TestLooseningPolicyDoesNotLoosenCorrectness is the line this change must not
// cross. A failed rehearsal, a blocking review and an unanswered rename
// question are the change being wrong, somebody objecting, and a column's data
// waiting on an answer — none of them is an opinion about process.
func TestLooseningPolicyDoesNotLoosenCorrectness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	scope, userID, requestID := gated(ctx, t, pool)
	if err := scope.SetPolicy(ctx, userID, store.Policy{
		ApprovalsRequired: 0,
	}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if !state.Executable {
		t.Fatalf("the fixture is not open to begin with: %s", state.Reason)
	}

	// A rehearsal that failed still shuts it, whatever the project asks for.
	if err := st.RecordProof(ctx, state.MigrationID, "failed",
		"the result was not the declared schema", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}
	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after a failed proof: %v", err)
	}
	if after.Executable {
		t.Error("a migration that failed its rehearsal is executable because the " +
			"project asked for less process")
	}
}
