package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// namedTable is table() under a name of the caller's choosing, which is the one
// thing a table rename needs to vary.
func namedTable(name string, columns ...schema.Column) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name:   "public",
		Tables: []schema.Table{{Name: name, Columns: columns}},
	}}}
}

// tableRenameFixture stands up a request whose branch renamed the whole table.
func tableRenameFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (
	scope *store.Scope, userID, requestID int64) {
	t.Helper()
	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool,
		namedTable("orders", text("id"), text("note")))
	scope = st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "renaming-table", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, namedTable("purchase", text("id"), text("note")))

	requestID, err = scope.MergeBranch(ctx, userID, branchID, databaseID, "rename table", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	return scope, userID, requestID
}

// TestATableRenameIsAQuestionBeforeItIsAMigration covers the hole this closes.
//
// Until now only columns raised a rename question. A renamed table arrived as a
// DROP and a CREATE, the gate found no unanswered rename to hold, and the
// request passed review looking clean while every row in the table was about to
// be discarded. The silence was the dangerous part: the column case at least
// stopped and asked.
func TestATableRenameIsAQuestionBeforeItIsAMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, _, requestID := tableRenameFixture(ctx, t, pool)

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames == 0 {
		t.Fatal("a whole table is being dropped and nobody is being asked " +
			"whether it was renamed")
	}
	if state.Executable {
		t.Error("the request is executable with the question still open")
	}
}

// TestConfirmingATableRenameKeepsTheRows is the payoff.
func TestConfirmingATableRenameKeepsTheRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := tableRenameFixture(ctx, t, pool)
	before := targetOf(ctx, t, pool, requestID)

	r := diff.Rename{Namespace: "public", From: "orders", To: "purchase"}
	if err := scope.AnswerRename(ctx, userID, requestID, r, true, "same table"); err != nil {
		t.Fatalf("AnswerRename: %v", err)
	}

	got := statementsOf(ctx, t, pool, requestID)
	if len(got) != 1 {
		t.Fatalf("expected a single rename, got %v", got)
	}
	if !strings.Contains(got[0], "RENAME TO purchase") {
		t.Errorf("wrong statement: %s", got[0])
	}
	for _, sql := range got {
		if strings.Contains(sql, "DROP TABLE") {
			t.Errorf("the table is still being discarded with all its rows: %s", sql)
		}
		if strings.Contains(sql, "CREATE TABLE") {
			t.Errorf("the table is still being recreated empty: %s", sql)
		}
	}

	// The end state is identical either way, so the migration declares the
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

// TestDecliningATableRenameSettlesItToo. Both answers close the question and
// only one changes the statements; the declining one is the more dangerous,
// which is why it is asked for rather than assumed.
func TestDecliningATableRenameSettlesItToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	scope, userID, requestID := tableRenameFixture(ctx, t, pool)

	r := diff.Rename{Namespace: "public", From: "orders", To: "purchase"}
	if err := scope.AnswerRename(ctx, userID, requestID, r, false, "genuinely new"); err != nil {
		t.Fatalf("AnswerRename: %v", err)
	}

	got := strings.Join(statementsOf(ctx, t, pool, requestID), "\n")
	if !strings.Contains(got, "DROP TABLE") {
		t.Errorf("declining should leave the drop in place: %s", got)
	}
	if strings.Contains(got, "RENAME TO") {
		t.Errorf("declined and renamed anyway: %s", got)
	}

	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.UnansweredRenames != 0 {
		t.Error("answering 'not a rename' must settle the question too")
	}
}
