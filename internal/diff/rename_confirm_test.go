package diff_test

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

func withCols(cols ...schema.Column) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name:   "public",
		Tables: []schema.Table{{Name: "orders", Columns: cols}},
	}}}
}

func kinds(changes []diff.Change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, string(c.Kind))
	}
	return out
}

// TestAnUnconfirmedRenameStaysADropAndAdd is the default, and the safe one.
// From the schemas alone the two are indistinguishable, so the engine proposes
// and never decides.
func TestAnUnconfirmedRenameStaysADropAndAdd(t *testing.T) {
	from := withCols(col("id", "bigint"), col("note", "text"))
	to := withCols(col("id", "bigint"), col("remark", "text"))

	r := diff.Compute(from, to)
	got := kinds(r.Changes)
	if len(got) != 2 {
		t.Fatalf("expected a drop and an add, got %v", got)
	}
	if len(r.Renames) != 1 {
		t.Fatalf("the pair should be proposed as a rename candidate, got %d", len(r.Renames))
	}
	if c := r.Renames[0]; c.From != "note" || c.To != "remark" {
		t.Errorf("candidate names %s → %s, want note → remark", c.From, c.To)
	}
}

// TestAConfirmedRenameKeepsTheData is what confirming is for.
func TestAConfirmedRenameKeepsTheData(t *testing.T) {
	from := withCols(col("id", "bigint"), col("note", "text"))
	to := withCols(col("id", "bigint"), col("remark", "text"))
	confirmed := []diff.Rename{{Namespace: "public", Table: "orders", From: "note", To: "remark"}}

	r := diff.ComputeWith(from, to, confirmed)
	if got := kinds(r.Changes); len(got) != 1 || got[0] != string(diff.RenameColumn) {
		t.Fatalf("expected one rename, got %v", got)
	}
	if r.Changes[0].Class != diff.MetadataOnly {
		t.Errorf("a rename touches no rows, got class %s", r.Changes[0].Class)
	}

	statements := render.Statements(r.Changes, from, to)
	if len(statements) != 1 {
		t.Fatalf("expected one statement, got %d", len(statements))
	}
	if statements[0].SQL != "ALTER TABLE public.orders RENAME COLUMN note TO remark;" {
		t.Errorf("wrong statement: %s", statements[0].SQL)
	}
	for _, s := range statements {
		if strings.Contains(s.SQL, "DROP COLUMN") {
			t.Errorf("a confirmed rename still discards the column: %s", s.SQL)
		}
	}
}

// TestAConfirmedRenameStillReportsWhatElseChanged: confirming a rename says the
// column is the same column, not that it is unchanged.
func TestAConfirmedRenameStillReportsWhatElseChanged(t *testing.T) {
	from := withCols(schema.Column{Name: "note", Type: "text", Nullable: true})
	to := withCols(schema.Column{Name: "remark", Type: "text", Nullable: false,
		Default: "''"})
	confirmed := []diff.Rename{{Namespace: "public", Table: "orders", From: "note", To: "remark"}}

	r := diff.ComputeWith(from, to, confirmed)
	got := kinds(r.Changes)
	var renamed, notNull, deflt bool
	for _, k := range got {
		switch k {
		case string(diff.RenameColumn):
			renamed = true
		case string(diff.SetNotNull):
			notNull = true
		case string(diff.SetDefault):
			deflt = true
		}
	}
	if !renamed || !notNull || !deflt {
		t.Errorf("expected the rename and what else changed with it, got %v", got)
	}

	// And the residual changes must name the column by its new name, or they
	// would run against one that no longer exists.
	for _, s := range render.Statements(r.Changes, from, to) {
		if strings.Contains(s.SQL, "note") && !strings.Contains(s.SQL, "RENAME") {
			t.Errorf("a statement after the rename still names the old column: %s", s.SQL)
		}
	}
}

// TestAStaleRenameIsIgnored: an answer kept from before the schemas moved must
// not act on names that are no longer there.
func TestAStaleRenameIsIgnored(t *testing.T) {
	// Somebody answered "note became remark", and then the rename was undone on
	// the other side — there is no remark to rename into any more.
	from := withCols(col("note", "text"))
	to := withCols(col("note", "text"), col("channel", "text"))
	confirmed := []diff.Rename{{Namespace: "public", Table: "orders", From: "note", To: "remark"}}

	r := diff.ComputeWith(from, to, confirmed)
	for _, c := range r.Changes {
		if c.Kind == diff.RenameColumn {
			t.Errorf("acted on a rename whose destination is not in the schema: %v",
				kinds(r.Changes))
		}
	}
}

// TestRenamingAndReusingTheOldName is the case the staleness guard must not
// swallow: the data moves to the new name and a fresh column takes the old one.
func TestRenamingAndReusingTheOldName(t *testing.T) {
	from := withCols(col("note", "text"))
	to := withCols(col("remark", "text"), col("note", "bigint"))
	confirmed := []diff.Rename{{Namespace: "public", Table: "orders", From: "note", To: "remark"}}

	statements := render.Statements(diff.ComputeWith(from, to, confirmed).Changes, from, to)
	var sql []string
	for _, s := range statements {
		sql = append(sql, s.SQL)
		if strings.Contains(s.SQL, "DROP COLUMN") {
			t.Errorf("the original column is discarded despite the rename: %s", s.SQL)
		}
	}
	if len(sql) != 2 {
		t.Fatalf("expected a rename and an add, got %v", sql)
	}
	if !strings.Contains(sql[0], "RENAME COLUMN note TO remark") {
		t.Errorf("the rename must come first, got %v", sql)
	}
	if !strings.Contains(sql[1], "ADD COLUMN note") {
		t.Errorf("the new column should take the freed name, got %v", sql)
	}
}
