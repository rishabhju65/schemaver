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

// TestEditingWithdrawsApproval is the guard on the hole that editing opens.
//
// Approvals used to be tied to a migration id and the fingerprint pair it
// spans, which was sufficient while the statements between those points could
// not change: regenerating made a new row and left the old approvals behind on
// one nobody would execute.
//
// An edit changes neither the id nor either fingerprint. Without a digest over
// the statements themselves, an administrator could edit an approved ALTER into
// a DROP and it would remain approved — same reviewers, same timestamps, gate
// open, endpoints still the ones that were reviewed. This test approves a
// migration, edits one character of it, and insists the gate shuts.
func TestEditingWithdrawsApproval(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the edit test")
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
		SELECT i.project_id,
		       (SELECT m.user_id FROM schemaver.project_member m
		         WHERE m.project_id = i.project_id AND m.role = 'admin' LIMIT 1),
		       (SELECT id FROM schemaver.database WHERE name = 'shop_prod'),
		       (SELECT id FROM schemaver.database WHERE name = 'shop_staging')
		  FROM schemaver.instance i LIMIT 1`).
		Scan(&projectID, &userID, &target, &source); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source, "edit probe", "")
	if err != nil {
		t.Skipf("propose: %v", err)
	}
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Skipf("generate: %v", err)
	}

	// Stand in for the prover and the reviewer, so the gate is open.
	if err := st.RecordProof(ctx, migrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}
	if err := st.RecordRevertProof(ctx, migrationID, "passed", ""); err != nil {
		t.Fatalf("RecordRevertProof: %v", err)
	}
	if err := scope.Decide(ctx, requestID, userID, "approve", "looks right"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	before, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if !before.Executable {
		t.Fatalf("the gate did not open before editing: %s", before.Reason)
	}

	// One character.
	detail, err := scope.Request(ctx, requestID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(detail.Steps) == 0 {
		t.Fatal("no statements to edit")
	}
	edited := detail.Steps[0].SQL + " -- adjusted by hand"
	if err := scope.EditStatement(ctx, userID, migrationID, false,
		detail.Steps[0].Ordinal, edited); err != nil {
		t.Fatalf("EditStatement: %v", err)
	}

	after, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after edit: %v", err)
	}
	if after.Executable {
		t.Error("the approval survived an edit to the approved statements — " +
			"a reviewer's judgement about one plan was counted for another")
	}
	if after.AdminApprovals != 0 {
		t.Errorf("%d approval(s) still counted after the statements changed",
			after.AdminApprovals)
	}
	if after.StaleDecisions == 0 {
		t.Error("the superseded decision vanished; it should be shown as no " +
			"longer applying rather than erased")
	}
	if after.PlanDigest == before.PlanDigest {
		t.Error("the plan digest did not change when a statement did")
	}
	if after.ProofState != "pending" {
		t.Errorf("proof state is %q after an edit, want pending: statements "+
			"nobody has checked must not carry a passing proof", after.ProofState)
	}
	t.Logf("after editing: %s", after.Reason)

	// And the edit is actually stored.
	detail, err = scope.Request(ctx, requestID)
	if err != nil {
		t.Fatalf("Request after edit: %v", err)
	}
	if !strings.Contains(detail.Steps[0].SQL, "adjusted by hand") {
		t.Errorf("the edit was not stored: %s", detail.Steps[0].SQL)
	}

	// A non-administrator cannot edit.
	var viewer int64
	if err := pool.QueryRow(ctx, `
		SELECT user_id FROM schemaver.project_member
		 WHERE project_id = $1 AND role <> 'admin' LIMIT 1`, projectID).
		Scan(&viewer); err == nil {
		if err := scope.EditStatement(ctx, viewer, migrationID, false, 1, "SELECT 1;"); !errors.Is(err, store.ErrNotAdmin) {
			t.Errorf("a non-administrator edit was not refused: %v", err)
		}
	}

	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.change_request WHERE id = $1`, requestID)
	})
}
