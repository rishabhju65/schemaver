package diff

import (
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

func base() *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{{
			Name: "orders",
			Columns: []schema.Column{
				{Name: "id", Type: "bigint"},
				{Name: "customer_id", Type: "bigint"},
				{Name: "total", Type: "numeric(12,2)", Nullable: true},
			},
			Constraints: []schema.Constraint{
				{Name: "orders_pkey", Type: schema.PrimaryKey, Columns: []string{"id"}},
			},
			Indexes: []schema.Index{
				{Name: "orders_customer_idx", Method: "btree", Columns: []string{"customer_id"}},
			},
		}},
		Enums: []schema.Enum{{Name: "status", Labels: []string{"new", "paid"}}},
	}}}
}

// mutate returns base with fn applied.
func mutate(fn func(*schema.Schema)) *schema.Schema {
	s := base()
	fn(s)
	return s
}

func kinds(r Result) []Kind {
	out := make([]Kind, len(r.Changes))
	for i, c := range r.Changes {
		out[i] = c.Kind
	}
	return out
}

func find(r Result, k Kind) *Change {
	for i := range r.Changes {
		if r.Changes[i].Kind == k {
			return &r.Changes[i]
		}
	}
	return nil
}

func TestIdenticalSchemasHaveNoChanges(t *testing.T) {
	if r := Compute(base(), base()); !r.Empty() {
		t.Errorf("identical schemas produced %d changes: %v", len(r.Changes), kinds(r))
	}
}

// TestOrderingIgnored guards the same property the fingerprint has: how a schema
// was assembled must not look like a change.
func TestOrderingIgnored(t *testing.T) {
	shuffled := mutate(func(s *schema.Schema) {
		t := &s.Namespaces[0].Tables[0]
		t.Columns[0], t.Columns[2] = t.Columns[2], t.Columns[0]
	})
	if r := Compute(base(), shuffled); !r.Empty() {
		t.Errorf("reordering was reported as change: %v", kinds(r))
	}
}

// TestCostClassification is the axis review actually cares about: not that
// something changed, but what it costs.
func TestCostClassification(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*schema.Schema)
		kind   Kind
		want   Class
	}{
		{"dropping a column destroys data", func(s *schema.Schema) {
			t := &s.Namespaces[0].Tables[0]
			t.Columns = t.Columns[:2]
		}, DropColumn, Destructive},

		{"dropping a table destroys data", func(s *schema.Schema) {
			s.Namespaces[0].Tables = nil
		}, DropTable, Destructive},

		{"changing a type rewrites the table", func(s *schema.Schema) {
			s.Namespaces[0].Tables[0].Columns[2].Type = "numeric(14,4)"
		}, AlterColumnType, Rewriting},

		{"requiring non-null scans every row", func(s *schema.Schema) {
			s.Namespaces[0].Tables[0].Columns[2].Nullable = false
		}, SetNotNull, LockHeavy},

		{"adding a constraint verifies every row", func(s *schema.Schema) {
			t := &s.Namespaces[0].Tables[0]
			t.Constraints = append(t.Constraints, schema.Constraint{
				Name: "orders_total_positive", Type: schema.Check, Expression: "total >= 0"})
		}, AddConstraint, LockHeavy},

		{"building an index blocks", func(s *schema.Schema) {
			t := &s.Namespaces[0].Tables[0]
			t.Indexes = append(t.Indexes, schema.Index{
				Name: "orders_total_idx", Method: "btree", Columns: []string{"total"}})
		}, CreateIndex, LockHeavy},

		{"adding a column is cheap", func(s *schema.Schema) {
			t := &s.Namespaces[0].Tables[0]
			t.Columns = append(t.Columns, schema.Column{Name: "note", Type: "text", Nullable: true})
		}, AddColumn, Additive},

		{"relaxing non-null is metadata", func(s *schema.Schema) {
			s.Namespaces[0].Tables[0].Columns[0].Nullable = true
		}, DropNotNull, MetadataOnly},

		{"dropping an index is metadata", func(s *schema.Schema) {
			s.Namespaces[0].Tables[0].Indexes = nil
		}, DropIndex, MetadataOnly},

		{"appending an enum value is cheap", func(s *schema.Schema) {
			e := &s.Namespaces[0].Enums[0]
			e.Labels = append(e.Labels, "shipped")
		}, AddEnumLabel, Additive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Compute(base(), mutate(tc.mutate))
			c := find(r, tc.kind)
			if c == nil {
				t.Fatalf("no %s change produced; got %v", tc.kind, kinds(r))
			}
			if c.Class != tc.want {
				t.Errorf("%s classified %s, want %s", tc.kind, c.Class, tc.want)
			}
			if c.Summary == "" {
				t.Error("no summary; review has nothing to display")
			}
		})
	}
}

// TestExecutionOrder checks the sequence is executable: teardown before buildup,
// and dependants either side of what they depend on.
func TestExecutionOrder(t *testing.T) {
	// Replace the table wholesale: drop the old, create a new one with an index
	// and a foreign key.
	next := mutate(func(s *schema.Schema) {
		ns := &s.Namespaces[0]
		ns.Tables = []schema.Table{{
			Name:    "shipments",
			Columns: []schema.Column{{Name: "id", Type: "bigint"}},
			Constraints: []schema.Constraint{{
				Name: "shipments_order_fk", Type: schema.ForeignKey,
				Columns: []string{"id"}, RefTable: "orders", RefColumns: []string{"id"},
			}},
			Indexes: []schema.Index{{
				Name: "shipments_id_idx", Method: "btree", Columns: []string{"id"}}},
		}}
	})

	r := Compute(base(), next)
	pos := map[Kind]int{}
	for i, c := range r.Changes {
		if _, seen := pos[c.Kind]; !seen {
			pos[c.Kind] = i
		}
	}

	for _, rule := range []struct{ first, then Kind }{
		{DropIndex, DropTable},       // an index cannot outlive its table
		{DropTable, CreateTable},     // teardown before buildup
		{CreateTable, AddConstraint}, // a foreign key needs both tables
		{CreateTable, CreateIndex},   // an index needs its table
	} {
		a, aok := pos[rule.first]
		b, bok := pos[rule.then]
		if !aok || !bok {
			continue
		}
		if a > b {
			t.Errorf("%s came after %s; the sequence is not executable", rule.first, rule.then)
		}
	}
}

// TestOrderIsDeterministic matters because D-007 regenerates migrations. A
// regenerated migration must be identical when nothing has changed, or every
// rebase would look like a new change.
func TestOrderIsDeterministic(t *testing.T) {
	next := mutate(func(s *schema.Schema) {
		t := &s.Namespaces[0].Tables[0]
		t.Columns = append(t.Columns,
			schema.Column{Name: "zeta", Type: "text", Nullable: true},
			schema.Column{Name: "alpha", Type: "text", Nullable: true})
		t.Indexes = append(t.Indexes,
			schema.Index{Name: "idx_z", Method: "btree", Columns: []string{"zeta"}},
			schema.Index{Name: "idx_a", Method: "btree", Columns: []string{"alpha"}})
	})

	first := Compute(base(), next)
	for i := 0; i < 20; i++ {
		again := Compute(base(), next)
		if len(again.Changes) != len(first.Changes) {
			t.Fatalf("change count varies between runs: %d then %d",
				len(first.Changes), len(again.Changes))
		}
		for j := range first.Changes {
			if first.Changes[j].ID != again.Changes[j].ID {
				t.Fatalf("order varies at %d: %s then %s",
					j, first.Changes[j].ID, again.Changes[j].ID)
			}
		}
	}
}

// TestChangeIDsAreSemantic covers what comment anchoring depends on (D-008): an
// id must describe the change, so it survives regeneration and reordering.
func TestChangeIDsAreSemantic(t *testing.T) {
	next := mutate(func(s *schema.Schema) {
		t := &s.Namespaces[0].Tables[0]
		t.Columns = t.Columns[:2]
	})

	c := find(Compute(base(), next), DropColumn)
	if c == nil {
		t.Fatal("no drop produced")
	}
	if c.ID != "drop_column:public.orders.total" {
		t.Errorf("id is %q; it should name the change, not its position", c.ID)
	}

	// The same change reached by a different route keeps the same identity.
	withExtra := mutate(func(s *schema.Schema) {
		t := &s.Namespaces[0].Tables[0]
		t.Columns = append(t.Columns[:2], schema.Column{Name: "aaa", Type: "text", Nullable: true})
	})
	again := find(Compute(base(), withExtra), DropColumn)
	if again == nil || again.ID != c.ID {
		t.Error("the same drop got a different id when other changes moved around it")
	}
}
