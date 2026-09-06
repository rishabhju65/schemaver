package schema

import "testing"

// fp is a test helper: fingerprint or fail.
func fp(t *testing.T, s *Schema) Version {
	t.Helper()
	v, err := Fingerprint(s)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return v
}

// base returns a schema exercising every construct the model carries.
func base() *Schema {
	return &Schema{Namespaces: []Namespace{{
		Name: "public",
		Tables: []Table{{
			Name: "orders",
			Columns: []Column{
				{Name: "id", Type: "bigint", Identity: "ALWAYS"},
				{Name: "customer_id", Type: "bigint"},
				{Name: "total", Type: "numeric(12,2)", Nullable: true},
			},
			Constraints: []Constraint{
				{Name: "orders_pkey", Type: PrimaryKey, Columns: []string{"id"}},
				{
					Name: "orders_customer_fk", Type: ForeignKey,
					Columns: []string{"customer_id"}, RefTable: "customers",
					RefColumns: []string{"id"}, OnDelete: "CASCADE",
				},
			},
			Indexes: []Index{{
				Name: "orders_customer_idx", Method: "btree",
				Columns: []string{"customer_id", "total"},
			}},
		}},
		Enums: []Enum{{Name: "status", Labels: []string{"pending", "paid", "shipped"}}},
	}}}
}

// TestFingerprintIgnoresInputOrder is the core guarantee: how a schema was
// assembled must not affect its identity.
func TestFingerprintIgnoresInputOrder(t *testing.T) {
	a := base()
	b := base()

	// Shuffle every collection whose order carries no meaning.
	tb := &b.Namespaces[0].Tables[0]
	tb.Columns[0], tb.Columns[2] = tb.Columns[2], tb.Columns[0]
	tb.Constraints[0], tb.Constraints[1] = tb.Constraints[1], tb.Constraints[0]

	if fp(t, a) != fp(t, b) {
		t.Error("fingerprint changed when only collection order differed")
	}
}

// TestFingerprintRespectsSignificantOrder guards the opposite failure: sorting
// away an order that carries meaning would equate schemas that behave
// differently.
func TestFingerprintRespectsSignificantOrder(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Schema)
	}{
		{"primary key column order", func(s *Schema) {
			c := &s.Namespaces[0].Tables[0].Constraints[0]
			c.Columns = []string{"customer_id", "id"}
		}},
		{"index column order", func(s *Schema) {
			i := &s.Namespaces[0].Tables[0].Indexes[0]
			i.Columns[0], i.Columns[1] = i.Columns[1], i.Columns[0]
		}},
		{"enum label order", func(s *Schema) {
			e := &s.Namespaces[0].Enums[0]
			e.Labels[0], e.Labels[2] = e.Labels[2], e.Labels[0]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			if fp(t, base()) == fp(t, m) {
				t.Errorf("%s was canonicalized away; it is semantically significant", tc.name)
			}
		})
	}
}

// TestFingerprintDetectsSemanticChange checks the model is not so aggressively
// normalized that real changes vanish.
func TestFingerprintDetectsSemanticChange(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Schema)
	}{
		{"added column", func(s *Schema) {
			tb := &s.Namespaces[0].Tables[0]
			tb.Columns = append(tb.Columns, Column{Name: "note", Type: "text", Nullable: true})
		}},
		{"changed type", func(s *Schema) {
			s.Namespaces[0].Tables[0].Columns[2].Type = "numeric(14,4)"
		}},
		{"dropped nullability", func(s *Schema) {
			s.Namespaces[0].Tables[0].Columns[2].Nullable = false
		}},
		{"changed on delete", func(s *Schema) {
			s.Namespaces[0].Tables[0].Constraints[1].OnDelete = "RESTRICT"
		}},
		{"index made unique", func(s *Schema) {
			s.Namespaces[0].Tables[0].Indexes[0].Unique = true
		}},
		{"added enum label", func(s *Schema) {
			e := &s.Namespaces[0].Enums[0]
			e.Labels = append(e.Labels, "cancelled")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			if fp(t, base()) == fp(t, m) {
				t.Errorf("%s did not change the fingerprint", tc.name)
			}
		})
	}
}

// TestFingerprintIgnoresEngineFormatting covers the changes that must NOT alter
// identity: incidental formatting, defaulted collation, and the engine's own
// rendering of an object.
func TestFingerprintIgnoresEngineFormatting(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Schema)
	}{
		{"whitespace in default", func(s *Schema) {
			s.Namespaces[0].Tables[0].Columns[2].Default = "  0.00  "
		}},
		{"rendered definition text", func(s *Schema) {
			s.Namespaces[0].Tables[0].Constraints[0].Definition = "PRIMARY KEY (id)"
		}},
		{"default collation", func(s *Schema) {
			s.Namespaces[0].Tables[0].Columns[1].Collation = "default"
		}},
		{"empty versus nil slice", func(s *Schema) {
			s.Namespaces[0].Tables[0].Indexes[0].Include = []string{}
		}},
		{"include column order", func(s *Schema) {
			s.Namespaces[0].Tables[0].Indexes[0].Include = []string{"total", "id"}
		}},
	}

	// The whitespace case needs a matching baseline: the value differs, only its
	// formatting must not.
	withDefault := base()
	withDefault.Namespaces[0].Tables[0].Columns[2].Default = "0.00"
	withInclude := base()
	withInclude.Namespaces[0].Tables[0].Indexes[0].Include = []string{"id", "total"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := base()
			switch tc.name {
			case "whitespace in default":
				want = withDefault
			case "include column order":
				want = withInclude
			}
			m := base()
			if tc.name == "whitespace in default" {
				m.Namespaces[0].Tables[0].Columns[2].Default = "0.00"
			}
			if tc.name == "include column order" {
				m.Namespaces[0].Tables[0].Indexes[0].Include = []string{"id", "total"}
			}
			tc.mutate(m)
			if fp(t, want) != fp(t, m) {
				t.Errorf("%s changed the fingerprint; it carries no schema meaning", tc.name)
			}
		})
	}
}

// TestFingerprintDoesNotMutate guards against the helper functions normalizing
// the caller's schema underneath them.
func TestFingerprintDoesNotMutate(t *testing.T) {
	s := base()
	before := s.Namespaces[0].Tables[0].Columns[0].Name
	_ = fp(t, s)
	if _, err := Canonical(s); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if got := s.Namespaces[0].Tables[0].Columns[0].Name; got != before {
		t.Errorf("caller's schema was reordered: first column %q became %q", before, got)
	}
}

// TestFingerprintIsStable pins the digest so an accidental change to the
// canonical form is caught rather than silently reversioning every schema in
// existence.
func TestFingerprintIsStable(t *testing.T) {
	const want = Version("33a9ca9ff8445b994317225ada9224421fee216d339a63b0a415e7458b6dbf4a")
	got := fp(t, base())
	if got != want {
		t.Errorf("canonical form changed: got %s, want %s", got, want)
	}
	t.Logf("baseline fingerprint: %s", got)
}
