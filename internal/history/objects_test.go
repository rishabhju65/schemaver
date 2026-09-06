package history

import (
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

func base() *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{
			{Name: "orders", Columns: []schema.Column{
				{Name: "id", Type: "bigint"},
				{Name: "total", Type: "numeric(12,2)", Nullable: true},
			}},
			{Name: "customers", Columns: []schema.Column{{Name: "id", Type: "bigint"}}},
		},
		Enums:     []schema.Enum{{Name: "status", Labels: []string{"new", "done"}}},
		Sequences: []schema.Sequence{{Name: "invoice_no", Increment: 1}},
	}}}
}

func find(changes []ObjectChange, name string) *ObjectChange {
	for i := range changes {
		if changes[i].Name == name {
			return &changes[i]
		}
	}
	return nil
}

func TestDetectsAddedRemovedModified(t *testing.T) {
	after := base()
	ns := &after.Namespaces[0]
	// Modify orders, remove customers, add shipments.
	ns.Tables[0].Columns = append(ns.Tables[0].Columns,
		schema.Column{Name: "note", Type: "text", Nullable: true})
	ns.Tables = append(ns.Tables[:1], schema.Table{
		Name: "shipments", Columns: []schema.Column{{Name: "id", Type: "bigint"}}})

	changes := ObjectsChanged(base(), after)

	for name, want := range map[string]Kind{
		"public.orders":    Modified,
		"public.customers": Removed,
		"public.shipments": Added,
	} {
		got := find(changes, name)
		if got == nil {
			t.Errorf("%s: not reported", name)
			continue
		}
		if got.Kind != want {
			t.Errorf("%s: got %s, want %s", name, got.Kind, want)
		}
	}

	if s := Count(changes); s.Added != 1 || s.Removed != 1 || s.Modified != 1 {
		t.Errorf("summary: %+v", s)
	}
}

func TestIdenticalSchemasProduceNoChanges(t *testing.T) {
	if changes := ObjectsChanged(base(), base()); len(changes) != 0 {
		t.Errorf("identical schemas reported %d changes: %+v", len(changes), changes)
	}
}

// TestOrderingDoesNotMatter guards the same property the fingerprint has: how a
// schema was assembled must not look like a change.
func TestOrderingDoesNotMatter(t *testing.T) {
	shuffled := base()
	ns := &shuffled.Namespaces[0]
	ns.Tables[0], ns.Tables[1] = ns.Tables[1], ns.Tables[0]
	ns.Tables[1].Columns[0], ns.Tables[1].Columns[1] = ns.Tables[1].Columns[1], ns.Tables[1].Columns[0]

	if changes := ObjectsChanged(base(), shuffled); len(changes) != 0 {
		t.Errorf("reordering was reported as change: %+v", changes)
	}
}

func TestEnumAndSequenceChanges(t *testing.T) {
	after := base()
	after.Namespaces[0].Enums[0].Labels = []string{"new", "done", "cancelled"}
	after.Namespaces[0].Sequences[0].Increment = 10

	changes := ObjectsChanged(base(), after)
	for _, name := range []string{"public.status", "public.invoice_no"} {
		got := find(changes, name)
		if got == nil || got.Kind != Modified {
			t.Errorf("%s: got %+v, want modified", name, got)
		}
	}
}

// TestOwnedSequencesAreNotReported checks a sequence backing an identity column
// is not counted separately — it belongs to the column, and reporting both would
// show two changes for one edit.
func TestOwnedSequencesAreNotReported(t *testing.T) {
	before := base()
	before.Namespaces[0].Sequences = append(before.Namespaces[0].Sequences,
		schema.Sequence{Name: "orders_id_seq", OwnedByTable: "orders", OwnedByColumn: "id"})

	after := base()
	after.Namespaces[0].Sequences = append(after.Namespaces[0].Sequences,
		schema.Sequence{Name: "orders_id_seq", OwnedByTable: "orders",
			OwnedByColumn: "id", Increment: 5})

	if changes := ObjectsChanged(before, after); len(changes) != 0 {
		t.Errorf("owned sequence reported as a change: %+v", changes)
	}
}

func TestNilSchemasAreHandled(t *testing.T) {
	if got := Count(ObjectsChanged(nil, base())); got.Added == 0 {
		t.Error("first observation should report every object as added")
	}
	if got := Count(ObjectsChanged(base(), nil)); got.Removed == 0 {
		t.Error("losing every object should report removals")
	}
	if changes := ObjectsChanged(nil, nil); len(changes) != 0 {
		t.Errorf("two absent schemas reported changes: %+v", changes)
	}
}

func TestSummaryDescribe(t *testing.T) {
	for _, tc := range []struct {
		s    Summary
		want string
	}{
		{Summary{}, "no object-level changes"},
		{Summary{Added: 2}, "2 added"},
		{Summary{Added: 1, Modified: 3, Removed: 2}, "1 added, 3 modified, 2 removed"},
	} {
		if got := tc.s.Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
}
