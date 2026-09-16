package diff

import (
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// tableRenamed returns base with its one table under a different name.
func tableRenamed(to string) *schema.Schema {
	s := base()
	s.Namespaces[0].Tables[0].Name = to
	return s
}

func candidateFor(r Result, from string) *RenameCandidate {
	for i := range r.Renames {
		if r.Renames[i].From == from {
			return &r.Renames[i]
		}
	}
	return nil
}

// TestATableRenameIsProposedNotApplied is the same guarantee the column case
// makes, for the case with far more to lose.
//
// A renamed table and a dropped-and-recreated one are identical from the
// schemas. Guessing wrong discards every row in the table rather than one
// column's worth, so the engine proposes and a person decides.
func TestATableRenameIsProposedNotApplied(t *testing.T) {
	r := Compute(base(), tableRenamed("purchase"))

	got := candidateFor(r, "orders")
	if got == nil {
		t.Fatal("no candidate proposed; a renamed table reads as a drop that " +
			"destroys every row, and nobody is asked")
	}
	if !got.OfTable() {
		t.Error("the candidate names a column; the subject here is the table")
	}
	if got.To != "purchase" || got.Confidence != Likely {
		t.Errorf("proposed %s → %s at %s, want orders → purchase at %s",
			got.From, got.To, got.Confidence, Likely)
	}
	if got.Question == "" {
		t.Error("no question posed; a person cannot resolve what they are not asked")
	}

	// Until it is answered the diff still reports what it actually saw.
	drop := find(r, DropTable)
	if drop == nil {
		t.Fatal("the drop vanished; a proposal must not silently rewrite the diff")
	}
	if drop.Class != Destructive {
		t.Errorf("the drop is classified %s; unconfirmed it still discards every row",
			drop.Class)
	}
	if find(r, CreateTable) == nil {
		t.Error("the create vanished")
	}
	if find(r, RenameTable) != nil {
		t.Error("a rename was applied without anybody confirming it")
	}
}

// TestAConfirmedTableRenameKeepsTheRows is what confirming is for.
func TestAConfirmedTableRenameKeepsTheRows(t *testing.T) {
	r := ComputeWith(base(), tableRenamed("purchase"), []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
	})

	rename := find(r, RenameTable)
	if rename == nil {
		t.Fatal("confirmed and still not renamed; the answer changed nothing")
	}
	if rename.From != "orders" || rename.To != "purchase" {
		t.Errorf("renamed %s → %s", rename.From, rename.To)
	}
	if rename.Class != MetadataOnly {
		t.Errorf("class %s; relabelling a catalogue entry touches no rows", rename.Class)
	}
	if d := find(r, DropTable); d != nil {
		t.Error("the drop survived the confirmation, so the rows are destroyed anyway")
	}
	if c := find(r, CreateTable); c != nil {
		t.Error("the table is still being created, so the rename produced a duplicate")
	}
	// Its indexes and constraints came with it; nothing is recreated.
	if find(r, CreateIndex) != nil || find(r, AddConstraint) != nil {
		t.Error("indexes or constraints are being rebuilt on a table that only " +
			"changed its name")
	}
}

// TestTheRenameIsOrderedBeforeAnythingIsCreated covers the collision.
//
// Renaming orders to purchase while a new orders arrives has exactly one safe
// order, and it is not the one the change list is built in.
func TestTheRenameIsOrderedBeforeAnythingIsCreated(t *testing.T) {
	next := tableRenamed("purchase")
	next.Namespaces[0].Tables = append(next.Namespaces[0].Tables, schema.Table{
		Name:    "orders",
		Columns: []schema.Column{{Name: "id", Type: "bigint"}},
	})

	r := ComputeWith(base(), next, []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
	})

	renameAt, createAt := -1, -1
	for i, c := range r.Changes {
		switch {
		case c.Kind == RenameTable:
			renameAt = i
		case c.Kind == CreateTable && c.Table == "orders":
			createAt = i
		}
	}
	if renameAt < 0 || createAt < 0 {
		t.Fatalf("expected both a rename and a create, got rename=%d create=%d",
			renameAt, createAt)
	}
	if renameAt > createAt {
		t.Error("the new orders is created before the old one is renamed away, " +
			"so the rename collides with a name that is now taken")
	}
}

// TestRenamingATableAndAColumnAreTwoQuestions is the nesting.
//
// Confirming that orders became purchase says nothing about what happened to
// the columns inside it. The engine compares the old table against the new one
// under its new name, so a column that also moved raises its own question
// rather than being folded silently into the first answer.
func TestRenamingATableAndAColumnAreTwoQuestions(t *testing.T) {
	next := tableRenamed("purchase")
	cols := next.Namespaces[0].Tables[0].Columns
	for i := range cols {
		if cols[i].Name == "total" {
			cols[i].Name = "amount"
		}
	}

	// Before the table rename is settled this is a whole table dropped and a
	// whole table created; the column question does not exist yet.
	first := Compute(base(), next)
	if c := candidateFor(first, "orders"); c == nil {
		t.Fatal("the table rename was not proposed")
	}
	if c := candidateFor(first, "total"); c != nil {
		t.Error("a column rename is being proposed across two different tables")
	}

	// With the table rename settled, the column question appears inside it.
	second := ComputeWith(base(), next, []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
	})
	col := candidateFor(second, "total")
	if col == nil {
		t.Fatal("the column rename inside the renamed table was never asked " +
			"about; confirming the table quietly discarded the column's data")
	}
	if col.Table != "purchase" {
		t.Errorf("the question names table %q; it should be asked against the "+
			"name the table now has", col.Table)
	}

	// And answering both keeps everything.
	third := ComputeWith(base(), next, []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
		{Namespace: "public", Table: "purchase", From: "total", To: "amount"},
	})
	if find(third, RenameTable) == nil || find(third, RenameColumn) == nil {
		t.Error("both were confirmed and both should be renames")
	}
	if find(third, DropColumn) != nil || find(third, DropTable) != nil {
		t.Error("something is still being dropped after both answers")
	}
}

// TestUnrelatedTablesAreNotProposedAsARename keeps the question rare enough to
// be read.
//
// A drop and a create that share no columns are a drop and a create. Asking
// about every such pair would put a question between somebody and their
// migration for changes that have nothing to do with each other.
func TestUnrelatedTablesAreNotProposedAsARename(t *testing.T) {
	next := base()
	next.Namespaces[0].Tables = []schema.Table{{
		Name: "audit_log",
		Columns: []schema.Column{
			{Name: "event", Type: "text"},
			{Name: "at", Type: "timestamptz"},
		},
	}}

	r := Compute(base(), next)
	if c := candidateFor(r, "orders"); c != nil {
		t.Errorf("proposed orders → %s; they share no columns and are not "+
			"plausibly the same table", c.To)
	}
	if find(r, DropTable) == nil || find(r, CreateTable) == nil {
		t.Error("the drop and create should both stand")
	}
}

// TestAnAnswerIsIgnoredOnceTheSchemasMoved matches the column rule: an answer
// kept from before must not rename a table that is not there.
func TestAnAnswerIsIgnoredOnceTheSchemasMoved(t *testing.T) {
	r := ComputeWith(base(), tableRenamed("purchase"), []Rename{
		{Namespace: "public", From: "invoices", To: "purchase"},
	})
	if find(r, RenameTable) != nil {
		t.Error("renamed a table that was never in the source schema")
	}
	if find(r, DropTable) == nil {
		t.Error("the real drop was lost to a stale answer")
	}
}

// fullTable is a table as a real one arrives: with a primary key and an index,
// both named after the table, which is what the engine does by default.
func fullTable(name string) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{{
			Name: name,
			Columns: []schema.Column{
				{Name: "id", Type: "bigint"},
				{Name: "customer_id", Type: "bigint"},
			},
			Constraints: []schema.Constraint{
				{Name: name + "_pkey", Type: schema.PrimaryKey, Columns: []string{"id"}},
			},
			Indexes: []schema.Index{
				{Name: name + "_customer_idx", Method: "btree", Columns: []string{"customer_id"}},
			},
		}},
	}}}
}

// TestEverythingElseComesAfterTheRename is the ordering the first version got
// wrong, and it broke the feature for every table anybody actually has.
//
// RENAME TO does not rename a table's indexes or constraints, so a renamed
// table arrives still carrying orders_pkey and orders_customer_idx while the
// target calls them purchase_pkey and purchase_customer_idx. The diff drops the
// old ones and adds the new ones — and every one of those changes names the
// table by the name it will have, because that is the name it is compared
// under. Run before the rename, they address a table that does not exist yet.
func TestEverythingElseComesAfterTheRename(t *testing.T) {
	r := ComputeWith(fullTable("orders"), fullTable("purchase"), []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
	})

	renameAt := -1
	for i, c := range r.Changes {
		if c.Kind == RenameTable {
			renameAt = i
			break
		}
	}
	if renameAt < 0 {
		t.Fatal("no rename")
	}
	for i, c := range r.Changes {
		if i == renameAt || c.Kind == RenameTable {
			continue
		}
		if c.Table != "purchase" {
			continue
		}
		if i < renameAt {
			t.Errorf("%s on %s is ordered at %d, before the rename at %d; at that "+
				"point no table has that name", c.Kind, c.Table, i, renameAt)
		}
	}
}

// TestRenamingIntoAnOccupiedNameIsRefused keeps a confirmed answer from
// producing a migration that cannot run.
//
// Nothing proposes this — a name held by a table in both schemas is never
// created, so it is never half of a candidate. It arrives from an answer kept
// across a change to the schemas, and honouring it produced a rename onto a
// table that was still standing, with the occupant silently altered to match
// rather than dropped.
func TestRenamingIntoAnOccupiedNameIsRefused(t *testing.T) {
	from := &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{
			{Name: "orders", Columns: []schema.Column{{Name: "id", Type: "bigint"}}},
			{Name: "purchase", Columns: []schema.Column{{Name: "old", Type: "text"}}},
		},
	}}}
	to := &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{
			{Name: "purchase", Columns: []schema.Column{{Name: "id", Type: "bigint"}}},
		},
	}}}

	r := ComputeWith(from, to, []Rename{
		{Namespace: "public", From: "orders", To: "purchase"},
	})
	if find(r, RenameTable) != nil {
		t.Error("renamed onto a name another table still holds")
	}
	if find(r, DropTable) == nil {
		t.Error("orders is neither renamed nor dropped, so it survives a " +
			"migration that says it goes")
	}
}
