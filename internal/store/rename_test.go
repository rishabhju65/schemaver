package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/store"
)

// renamed builds a request where one column was dropped and another added with
// the same type — the shape a rename and a drop-plus-add both produce.
func renameFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (
	scope *store.Scope, userID, requestID int64) {
	t.Helper()
	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool,
		table(text("id"), text("note")))
	scope = st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "renaming", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("remark")))

	requestID, err = scope.MergeBranch(ctx, userID, branchID, databaseID, "rename", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	return scope, userID, requestID
}

func statementsOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, requestID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT st.sql FROM schemaver.migration_step st
		  JOIN schemaver.migration m ON m.id = st.migration_id
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL
		 ORDER BY st.ordinal`, requestID)
	if err != nil {
		t.Fatalf("read statements: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, sql)
	}
	return out
}

func targetOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, requestID int64) string {
	t.Helper()
	var to string
	if err := pool.QueryRow(ctx, `
		SELECT to_fingerprint FROM schemaver.migration
		 WHERE change_request_id = $1 AND superseded_at IS NULL`, requestID).Scan(&to); err != nil {
		t.Fatalf("read the declared target: %v", err)
	}
	return to
}

// TestARenameQuestionIsRaisedOnAMergedBranch covers a hole that let a column's
// data be discarded without anybody being asked.
//
// The candidates were computed only on the database-to-database path. A branch
// merge and a written change recorded an empty list, so the gate had nothing to
// hold and the drop went through as itself.
func TestARenameQuestionIsRaisedOnAMergedBranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, _, requestID := renameFixture(ctx, t, pool)
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames == 0 {
		t.Fatal("a column was dropped and another added with its type, and " +
			"nobody was asked whether that is a rename")
	}
	if state.Executable {
		t.Error("executable with an unanswered rename question")
	}

	// With the way back written, the rename question is what is left, and the
	// gate has to say so — it reports one thing at a time, in the order a
	// person can act on them.
	// Standing in for the worker, which is not running here.
	st := store.New(pool, nil)
	if err := st.RecordProof(ctx, state.MigrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}

	state, err = scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after the revert: %v", err)
	}
	if !strings.Contains(state.Reason, "rename") {
		t.Errorf("the gate should say it is waiting on the rename, got %q", state.Reason)
	}
}

// TestConfirmingARenameKeepsTheData is what the answer is for.
func TestConfirmingARenameKeepsTheData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := renameFixture(ctx, t, pool)
	before := targetOf(ctx, t, pool, requestID)

	r := diff.Rename{Namespace: "public", Table: "orders", From: "note", To: "remark"}
	if err := scope.AnswerRename(ctx, userID, requestID, r, true, "same column"); err != nil {
		t.Fatalf("AnswerRename: %v", err)
	}

	got := statementsOf(ctx, t, pool, requestID)
	if len(got) != 1 {
		t.Fatalf("expected a single rename, got %v", got)
	}
	if !strings.Contains(got[0], "RENAME COLUMN note TO remark") {
		t.Errorf("wrong statement: %s", got[0])
	}
	for _, sql := range got {
		if strings.Contains(sql, "DROP COLUMN") {
			t.Errorf("the column is still being discarded: %s", sql)
		}
	}

	// The end state is the same either way, so the migration still declares the
	// schema it always did and the rehearsal is unaffected.
	if after := targetOf(ctx, t, pool, requestID); after != before {
		t.Errorf("the declared target moved when the question was answered: %s → %s",
			before[:12], after[:12])
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames != 0 {
		t.Error("the question is still counted as open after being answered")
	}
}

// TestDecliningARenameSettlesItToo. Both answers close the question; only one
// changes the statements. The declining one is the more dangerous, which is why
// it is asked for rather than assumed.
func TestDecliningARenameSettlesItToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := renameFixture(ctx, t, pool)
	r := diff.Rename{Namespace: "public", Table: "orders", From: "note", To: "remark"}
	if err := scope.AnswerRename(ctx, userID, requestID, r, false, "different meaning"); err != nil {
		t.Fatalf("AnswerRename: %v", err)
	}

	got := statementsOf(ctx, t, pool, requestID)
	var dropped, added bool
	for _, sql := range got {
		if strings.Contains(sql, "DROP COLUMN note") {
			dropped = true
		}
		if strings.Contains(sql, "ADD COLUMN remark") {
			added = true
		}
	}
	if !dropped || !added {
		t.Errorf("declining should leave the drop and the add, got %v", got)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames != 0 {
		t.Error("declining did not settle the question")
	}

	// And it is on the record with a name against it, because this is the
	// answer that discards data.
	answers, err := scope.RenameAnswers(ctx, requestID)
	if err != nil {
		t.Fatalf("RenameAnswers: %v", err)
	}
	if len(answers) != 1 || answers[0].Renamed {
		t.Fatalf("expected one declining answer, got %+v", answers)
	}
	if answers[0].By == "" {
		t.Error("the answer records nobody")
	}
}

// TestChangingAnAnswerRebuildsThePlan: somebody can be wrong once.
func TestChangingAnAnswerRebuildsThePlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := renameFixture(ctx, t, pool)
	r := diff.Rename{Namespace: "public", Table: "orders", From: "note", To: "remark"}
	if err := scope.AnswerRename(ctx, userID, requestID, r, false, ""); err != nil {
		t.Fatalf("AnswerRename: %v", err)
	}
	if err := scope.AnswerRename(ctx, userID, requestID, r, true, "changed my mind"); err != nil {
		t.Fatalf("AnswerRename again: %v", err)
	}

	got := statementsOf(ctx, t, pool, requestID)
	if len(got) != 1 || !strings.Contains(got[0], "RENAME COLUMN") {
		t.Errorf("the plan did not follow the second answer, got %v", got)
	}
	answers, err := scope.RenameAnswers(ctx, requestID)
	if err != nil {
		t.Fatalf("RenameAnswers: %v", err)
	}
	if len(answers) != 1 {
		t.Errorf("changing an answer should replace it, not stack a second, got %d",
			len(answers))
	}
}

// TestAnotherTenantCannotAnswer.
func TestAnotherTenantCannotAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := renameFixture(ctx, t, pool)
	st := store.New(pool, nil)

	var otherOrg, otherProject int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO schemaver.organization (name) VALUES ('rename-iso') RETURNING id`).
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
		VALUES ('rename-iso', $1) RETURNING id`, otherOrg).Scan(&otherProject); err != nil {
		t.Fatalf("create the other project: %v", err)
	}

	r := diff.Rename{Namespace: "public", Table: "orders", From: "note", To: "remark"}
	if err := st.ForProject(otherProject).AnswerRename(ctx, userID, requestID, r, true, ""); err == nil {
		t.Error("another tenant answered a question on a request it cannot see")
	}

	// And the request is untouched by the attempt.
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames == 0 {
		t.Error("the question was settled by somebody with no claim to it")
	}
}
