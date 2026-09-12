package store

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/diff"
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
	steps, _ := buildRevert(forward, from, to)
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
	steps, _ := buildRevert(forward, from, to)
	if len(steps) == 0 {
		t.Fatal("no revert generated")
	}
	for _, st := range steps {
		if st.StructureOnly {
			t.Errorf("undoing a purely additive migration was marked lossy: %s", st.SQL)
		}
	}
}
