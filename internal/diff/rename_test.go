package diff

import (
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// renameOf returns base with one column swapped for another.
func renamed(from, to, typ string) *schema.Schema {
	s := base()
	t := &s.Namespaces[0].Tables[0]
	var kept []schema.Column
	for _, c := range t.Columns {
		if c.Name != from {
			kept = append(kept, c)
		}
	}
	t.Columns = append(kept, schema.Column{Name: to, Type: typ, Nullable: true})
	return s
}

// TestRenameIsProposedNotApplied is the guarantee that protects data. From the
// schema alone a rename and a drop-plus-add are identical, and guessing wrong
// destroys a column. The diff must surface the ambiguity and still report the
// drop and add it actually saw.
func TestRenameIsProposedNotApplied(t *testing.T) {
	r := Compute(base(), renamed("total", "amount", "numeric(12,2)"))

	if len(r.Renames) != 1 {
		t.Fatalf("got %d rename candidates, want 1", len(r.Renames))
	}
	got := r.Renames[0]
	if got.From != "total" || got.To != "amount" {
		t.Errorf("proposed %s → %s, want total → amount", got.From, got.To)
	}
	if got.Confidence != Likely {
		t.Errorf("confidence %s, want %s for a single drop and add", got.Confidence, Likely)
	}
	if got.Question == "" {
		t.Error("no question posed; a human cannot resolve what they are not asked")
	}

	// The underlying changes must still be present and still destructive.
	drop := find(r, DropColumn)
	if drop == nil {
		t.Fatal("the drop vanished; a proposal must not silently rewrite the diff")
	}
	if drop.Class != Destructive {
		t.Errorf("the drop is classified %s; until the rename is confirmed it still discards data", drop.Class)
	}
	if find(r, AddColumn) == nil {
		t.Error("the add vanished")
	}
}

// TestAmbiguousRenamesAreMarkedPossible checks that a table with several drops
// and adds does not present a guess as a confident answer.
func TestAmbiguousRenamesAreMarkedPossible(t *testing.T) {
	next := base()
	tb := &next.Namespaces[0].Tables[0]
	tb.Columns = []schema.Column{
		{Name: "id", Type: "bigint"},
		{Name: "buyer_id", Type: "bigint"},
		{Name: "seller_id", Type: "bigint"},
	}

	r := Compute(base(), next)
	if len(r.Renames) == 0 {
		t.Fatal("no candidates proposed")
	}
	for _, c := range r.Renames {
		if c.Confidence != Possible {
			t.Errorf("%s → %s marked %s; with several candidates any pairing is a guess",
				c.From, c.To, c.Confidence)
		}
	}
}

// TestTypeMismatchIsNotARename checks the heuristic does not pair columns that
// cannot be the same column.
func TestTypeMismatchIsNotARename(t *testing.T) {
	r := Compute(base(), renamed("total", "note", "text"))
	for _, c := range r.Renames {
		if c.From == "total" && c.To == "note" {
			t.Error("a numeric and a text column were proposed as a rename")
		}
	}
}

// TestNoRenameAcrossTables checks candidates stay within one table; a column
// vanishing here and appearing there is not a rename.
func TestNoRenameAcrossTables(t *testing.T) {
	next := base()
	ns := &next.Namespaces[0]
	ns.Tables[0].Columns = ns.Tables[0].Columns[:2]
	ns.Tables = append(ns.Tables, schema.Table{
		Name:    "archive",
		Columns: []schema.Column{{Name: "total", Type: "numeric(12,2)", Nullable: true}},
	})

	for _, c := range Compute(base(), next).Renames {
		if c.Table != "orders" {
			t.Errorf("candidate proposed across tables: %s.%s", c.Table, c.From)
		}
	}
}

func TestNoRenamesWhenNothingDropped(t *testing.T) {
	next := base()
	tb := &next.Namespaces[0].Tables[0]
	tb.Columns = append(tb.Columns, schema.Column{Name: "note", Type: "text", Nullable: true})

	if r := Compute(base(), next); len(r.Renames) != 0 {
		t.Errorf("proposed %d renames when only an add happened", len(r.Renames))
	}
}

// TestSummaryFlagsUnsafeChanges covers the signal review leads with.
func TestSummaryFlagsUnsafeChanges(t *testing.T) {
	additive := base()
	additive.Namespaces[0].Tables[0].Columns = append(
		additive.Namespaces[0].Tables[0].Columns,
		schema.Column{Name: "note", Type: "text", Nullable: true})

	if s := Compute(base(), additive).Summary; !s.Safe() {
		t.Errorf("an added nullable column reported unsafe: %+v", s)
	}

	dropped := base()
	dropped.Namespaces[0].Tables[0].Columns = dropped.Namespaces[0].Tables[0].Columns[:2]
	s := Compute(base(), dropped).Summary
	if s.Safe() {
		t.Error("a dropped column reported safe")
	}
	if s.Destructive != 1 {
		t.Errorf("destructive count %d, want 1", s.Destructive)
	}
}
