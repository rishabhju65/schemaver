package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAPassingRehearsalDoesNotClaimReadinessTheGateDenies covers a page that
// contradicted itself.
//
// A rehearsal that passed moved the request to READY_TO_EXECUTE — "approved and
// queued" — on the reasoning that it had been approved to get that far. Approval
// is one of the gate's conditions and not the only one, so the same page could
// show that label above a gate saying the change could not run and naming
// something nobody had done. Nothing was queued either.
func TestAPassingRehearsalDoesNotClaimReadinessTheGateDenies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "gate-probe", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("channel")))
	requestID, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "gate", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}

	// A lower environment that has not had this change. Approval and a passing
	// rehearsal are both satisfied; the promotion chain is not, which is the
	// ordinary shape of a change that is ready in every sense except the one
	// that matters.
	var instanceID, peerID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
		VALUES ($1, 'gate_peer', true,
		        (SELECT current_fingerprint FROM schemaver.database WHERE id = $2))
		RETURNING id`, instanceID, databaseID).Scan(&peerID); err != nil {
		t.Fatalf("create the lower environment: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		databaseID, peerID); err != nil {
		t.Fatalf("set the peer: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `UPDATE schemaver.database SET expected_peer_id = NULL WHERE id = $1`, databaseID)
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE id = $1`, peerID)
	})

	if err := scope.Decide(ctx, requestID, userID, "approve", "ship it"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := st.RecordProof(ctx, state.MigrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}

	var label, reason string
	if err := pool.QueryRow(ctx, `
		SELECT state, COALESCE(state_reason, '') FROM schemaver.change_request
		 WHERE id = $1`, requestID).Scan(&label, &reason); err != nil {
		t.Fatalf("read the state: %v", err)
	}

	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after the rehearsal: %v", err)
	}
	if after.Executable {
		t.Fatal("the gate opened without the change having been through the " +
			"environment below; this test proves nothing")
	}
	if label == "READY_TO_EXECUTE" {
		t.Errorf("the request claims to be ready while the gate says %q", after.Reason)
	}
	if reason == "" {
		t.Error("the request says nothing about what it is waiting for")
	}
	if !strings.Contains(reason, "gate_peer") {
		t.Errorf("the reason should be the gate's own, got %q", reason)
	}
}

// TestAPassingRehearsalSaysReadyWhenTheGateAgrees is the other half: the state
// must still move when there is genuinely nothing left to do.
func TestAPassingRehearsalSaysReadyWhenTheGateAgrees(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "gate-open", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("channel")))
	requestID, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "gate open", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}

	// Everything the gate asks for.
	if err := scope.Decide(ctx, requestID, userID, "approve", "ship it"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := st.RecordProof(ctx, state.MigrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}

	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after: %v", err)
	}
	var label string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM schemaver.change_request WHERE id = $1`, requestID).
		Scan(&label); err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if after.Executable && label != "READY_TO_EXECUTE" {
		t.Errorf("the gate is open and the request says %q", label)
	}
	if !after.Executable && label == "READY_TO_EXECUTE" {
		t.Errorf("the request claims readiness the gate denies: %s", after.Reason)
	}
	t.Logf("gate executable=%v, state %s", after.Executable, label)
}
