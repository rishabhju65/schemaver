package diff_test

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
)

func col(name, typ string) schema.Column {
	return schema.Column{Name: name, Type: typ, Nullable: true}
}

// orders builds a one-table schema, so a fixture reads as the divergence it
// describes rather than as nested struct literals.
func orders(columns ...schema.Column) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name:   "public",
		Tables: []schema.Table{{Name: "orders", Columns: columns}},
	}}}
}

// columnsOf reads back the merged column names, which is what every assertion
// here is really about.
func columnsOf(t *testing.T, s *schema.Schema) []string {
	t.Helper()
	if s == nil {
		t.Fatal("no merged schema")
	}
	var out []string
	for _, ns := range s.Namespaces {
		for _, tbl := range ns.Tables {
			for _, c := range tbl.Columns {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestMergeKeepsBothSidesWork is the property the whole feature exists for.
// Comparing the two schemas directly would render our own column as a DROP.
func TestMergeKeepsBothSidesWork(t *testing.T) {
	m := diff.ThreeWay(
		orders(col("id", "bigint")),
		orders(col("id", "bigint"), col("audit_ref", "text")),
		orders(col("id", "bigint"), col("channel", "text")),
	)
	if !m.Clean() {
		t.Fatalf("different columns are not a conflict: %v", m.Conflicts)
	}
	got := columnsOf(t, m.Schema)
	for _, want := range []string{"id", "audit_ref", "channel"} {
		if !has(got, want) {
			t.Errorf("merged schema lost %s; has %v", want, got)
		}
	}
}

// TestMergeReportsRatherThanGuesses: both sides gave the same column a
// different type, and no rule recovers which was meant.
func TestMergeReportsRatherThanGuesses(t *testing.T) {
	m := diff.ThreeWay(
		orders(col("id", "bigint")),
		orders(col("id", "bigint"), col("note", "character varying(50)")),
		orders(col("id", "bigint"), col("note", "text")),
	)
	if m.Clean() {
		t.Fatal("two different types for one column is a conflict")
	}
	if m.Schema != nil {
		t.Error("a conflicted merge must not name a result")
	}
	if got := m.Conflicts[0].Object; got != "public.orders.note" {
		t.Errorf("conflict object = %q, want public.orders.note", got)
	}
	// The line has to say what to choose between, not merely that there is a
	// choice.
	line := m.Conflicts[0].Describe()
	if !strings.Contains(line, "text") || !strings.Contains(line, "character varying(50)") {
		t.Errorf("conflict should name both versions, got %q", line)
	}
}

// TestIdenticalWorkIsNotAConflict: two people adding the same column is
// agreement reached separately, not a disagreement.
func TestIdenticalWorkIsNotAConflict(t *testing.T) {
	m := diff.ThreeWay(
		orders(col("id", "bigint")),
		orders(col("id", "bigint"), col("channel", "text")),
		orders(col("id", "bigint"), col("channel", "text")),
	)
	if !m.Clean() {
		t.Fatalf("the same change on both sides is not a conflict: %v", m.Conflicts)
	}
	if got := columnsOf(t, m.Schema); len(got) != 2 {
		t.Errorf("the shared column should appear once, got %v", got)
	}
}

// TestMergeOfAnUntouchedSideIsTheirsEntirely: where only one side moved, the
// merge is that side, and this is the ordinary bring-into-line case.
func TestMergeOfAnUntouchedSideIsTheirsEntirely(t *testing.T) {
	base := orders(col("id", "bigint"))
	m := diff.ThreeWay(base, base, orders(col("id", "bigint"), col("channel", "text")))
	if !m.Clean() {
		t.Fatalf("only one side moved: %v", m.Conflicts)
	}
	if got := columnsOf(t, m.Schema); !has(got, "channel") {
		t.Errorf("merged schema should have taken their work, got %v", got)
	}
}

// TestDropAgainstChangeConflicts: one side removed what the other retyped.
// Taking either silently loses a decision somebody made.
func TestDropAgainstChangeConflicts(t *testing.T) {
	m := diff.ThreeWay(
		orders(col("id", "bigint"), col("legacy", "text")),
		orders(col("id", "bigint")),
		orders(col("id", "bigint"), col("legacy", "jsonb")),
	)
	if m.Clean() {
		t.Fatal("dropping what the other side retyped is a conflict")
	}
	line := m.Conflicts[0].Describe()
	if !strings.Contains(line, "dropped it") {
		t.Errorf("conflict should say one side dropped it, got %q", line)
	}
}

// TestMergeDropsWhatOneSideRemovedAndTheOtherLeftAlone: a drop nobody contested
// is still a drop. The merge must not resurrect it.
func TestMergeDropsWhatOneSideRemovedAndTheOtherLeftAlone(t *testing.T) {
	base := orders(col("id", "bigint"), col("legacy", "text"))
	m := diff.ThreeWay(base, base, orders(col("id", "bigint")))
	if !m.Clean() {
		t.Fatalf("an uncontested drop is not a conflict: %v", m.Conflicts)
	}
	if got := columnsOf(t, m.Schema); has(got, "legacy") {
		t.Errorf("the dropped column came back: %v", got)
	}
}

// TestMergeIsTheMigrationTarget ties the merge to what it is for: the
// statements are the difference between where we are and the merged result, and
// that result is what the shadow proof checks against.
func TestMergeIsTheMigrationTarget(t *testing.T) {
	ours := orders(col("id", "bigint"), col("audit_ref", "text"))
	m := diff.ThreeWay(orders(col("id", "bigint")), ours,
		orders(col("id", "bigint"), col("channel", "text")))
	if !m.Clean() {
		t.Fatalf("unexpected conflicts: %v", m.Conflicts)
	}
	result := diff.Compute(ours, m.Schema)
	if len(result.Changes) != 1 {
		t.Fatalf("expected one statement's worth of change, got %v", result.Changes)
	}
	if !strings.Contains(result.Changes[0].Summary, "channel") {
		t.Errorf("the change should add their column, got %q", result.Changes[0].Summary)
	}
	// And nothing of ours is discarded on the way.
	for _, c := range result.Changes {
		if strings.Contains(c.Summary, "audit_ref") {
			t.Errorf("our own work is in the migration: %q", c.Summary)
		}
	}
}

// TestNewTablesFromBothSidesSurvive: the same property one level up.
func TestNewTablesFromBothSidesSurvive(t *testing.T) {
	table := func(name string) schema.Table {
		return schema.Table{Name: name, Columns: []schema.Column{col("id", "bigint")}}
	}
	with := func(tables ...schema.Table) *schema.Schema {
		return &schema.Schema{Namespaces: []schema.Namespace{{Name: "public", Tables: tables}}}
	}
	m := diff.ThreeWay(
		with(table("orders")),
		with(table("orders"), table("refunds")),
		with(table("orders"), table("shipments")),
	)
	if !m.Clean() {
		t.Fatalf("different new tables are not a conflict: %v", m.Conflicts)
	}
	var names []string
	for _, tbl := range m.Schema.Namespaces[0].Tables {
		names = append(names, tbl.Name)
	}
	if len(names) != 3 {
		t.Errorf("expected all three tables, got %v", names)
	}
}
