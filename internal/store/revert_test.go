package store

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

func table(name string, columns ...schema.Column) schema.Table {
	return schema.Table{Name: name, Columns: columns}
}

func col(name, typ string) schema.Column {
	return schema.Column{Name: name, Type: typ, Nullable: true}
}

func withTables(ts ...schema.Table) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{Name: "public", Tables: ts}}}
}

// TestRevertMarksWhatItCannotRestore is the whole point of generating a revert.
//
// Undoing `ADD COLUMN` is a real undo. Undoing `DROP COLUMN` is a statement that
// recreates an empty column and looks exactly like one — same shape, same
// syntax, and nothing on the page to tell them apart unless we put it there. A
// reviewer reading an unmarked list of revert statements concludes there is a
// way back from a change that has none, and finds out otherwise after the data
// is gone.
func TestRevertMarksWhatItCannotRestore(t *testing.T) {
	// Forward: drop a column, add another. Only the drop destroys anything.
	from := withTables(table("orders",
		col("id", "bigint"), col("legacy_status", "text")))
	to := withTables(table("orders",
		col("id", "bigint"), col("channel", "text")))

	forward := diff.Compute(from, to)
	steps, _ := buildRevert(forward, render.Statements(forward.Changes, from, to), from, to)
	if len(steps) == 0 {
		t.Fatal("no revert was generated for a reversible-looking change")
	}

	var restoresLost, dropsAdded bool
	for _, st := range steps {
		t.Logf("%d. %s  structure_only=%v", st.Ordinal, st.SQL, st.StructureOnly)
		switch {
		case strings.Contains(st.SQL, "legacy_status"):
			restoresLost = true
			if !st.StructureOnly {
				t.Error("bringing back a dropped column is marked as a full undo; " +
					"it restores an empty column and the contents are gone")
			}
			if st.Note == "" {
				t.Error("nothing on the statement says why it is not a real undo")
			}
		case strings.Contains(st.SQL, "channel"):
			dropsAdded = true
			if st.StructureOnly {
				t.Error("dropping a column the migration added is a complete undo " +
					"and must not be marked as losing data")
			}
		}
	}
	if !restoresLost {
		t.Error("the revert does not bring back the dropped column at all")
	}
	if !dropsAdded {
		t.Error("the revert does not remove the added column")
	}
}

// TestRevertOfAnAdditiveChangeLosesNothing guards the other direction: marking
// everything as lossy would be as useless as marking nothing.
func TestRevertOfAnAdditiveChangeLosesNothing(t *testing.T) {
	from := withTables(table("orders", col("id", "bigint")))
	to := withTables(table("orders", col("id", "bigint"), col("channel", "text")))

	forward := diff.Compute(from, to)
	steps, _ := buildRevert(forward, render.Statements(forward.Changes, from, to), from, to)
	if len(steps) == 0 {
		t.Fatal("no revert generated")
	}
	for _, st := range steps {
		if st.StructureOnly {
			t.Errorf("undoing a purely additive migration was marked lossy: %s", st.SQL)
		}
	}
}

// TestSliceRevertTakesOnlyWhatApplied covers the rollback that matters: the one
// offered after a migration stops partway.
//
// The generated revert assumes the target was reached. A run that applied two
// of three statements needs the revert of those two and must not touch the
// third, whose forward statement never ran — dropping an index that was never
// created fails, and failing during a rollback is the worst place to fail.
func TestSliceRevertTakesOnlyWhatApplied(t *testing.T) {
	from := withTables(table("orders",
		col("id", "bigint"), col("legacy_status", "text")))
	to := withTables(table("orders",
		col("id", "bigint"), col("channel", "text")))

	forward := diff.Compute(from, to)
	forwardStatements := render.Statements(forward.Changes, from, to)
	steps, _ := buildRevert(forward, forwardStatements, from, to)

	if !Sliceable(steps) {
		t.Fatalf("the revert cannot be sliced; every step needs a forward statement:\n%+v", steps)
	}

	full := SliceRevert(steps, len(forwardStatements))
	if len(full) != len(steps) {
		t.Errorf("slicing the whole migration returned %d of %d steps", len(full), len(steps))
	}

	// Nothing applied: nothing to undo.
	if got := SliceRevert(steps, 0); len(got) != 0 {
		t.Errorf("a run that applied nothing produced %d revert statement(s)", len(got))
	}

	// Only the first forward statement applied.
	partial := SliceRevert(steps, 1)
	if len(partial) == 0 {
		t.Fatal("a run that applied one statement produced no rollback at all")
	}
	if len(partial) >= len(steps) {
		t.Errorf("a partial rollback took %d of %d statements; it should take fewer",
			len(partial), len(steps))
	}
	for _, st := range partial {
		if st.Undoes > 1 {
			t.Errorf("the partial rollback includes a statement undoing forward "+
				"statement %d, which never ran: %s", st.Undoes, st.SQL)
		}
		t.Logf("undoes forward %d: %s", st.Undoes, st.SQL)
	}

	// Order within a slice follows the revert, not the forward migration.
	for i := 1; i < len(full); i++ {
		if full[i].Ordinal <= full[i-1].Ordinal {
			t.Errorf("slicing reordered the revert: %d came after %d",
				full[i].Ordinal, full[i-1].Ordinal)
		}
	}
}
