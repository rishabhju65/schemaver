package schema

import (
	"sort"
	"strings"
)

// Normalize rewrites s into canonical form in place.
//
// The contract: two schemas that are logically identical must be byte-identical
// after normalization. Everything the engine reports that is an artifact of
// *how* a schema was built rather than *what* it is — collection order, physical
// column position, incidental whitespace, defaulted collations — is erased here.
//
// Order is preserved only where order carries meaning. Key columns, index
// columns, and enum labels are semantically ordered and are never sorted;
// sorting them would silently equate schemas that behave differently.
func Normalize(s *Schema) {
	for i := range s.Namespaces {
		normalizeNamespace(&s.Namespaces[i])
	}
	sort.Slice(s.Namespaces, func(i, j int) bool {
		return s.Namespaces[i].Name < s.Namespaces[j].Name
	})
	s.Namespaces = nilIfEmpty(s.Namespaces)
}

func normalizeNamespace(n *Namespace) {
	n.Comment = strings.TrimSpace(n.Comment)

	for i := range n.Tables {
		normalizeTable(&n.Tables[i])
	}
	for i := range n.Sequences {
		normalizeSequence(&n.Sequences[i])
	}
	for i := range n.Enums {
		normalizeEnum(&n.Enums[i])
	}

	sort.Slice(n.Tables, func(i, j int) bool { return n.Tables[i].Name < n.Tables[j].Name })
	sort.Slice(n.Sequences, func(i, j int) bool { return n.Sequences[i].Name < n.Sequences[j].Name })
	sort.Slice(n.Enums, func(i, j int) bool { return n.Enums[i].Name < n.Enums[j].Name })

	n.Tables = nilIfEmpty(n.Tables)
	n.Sequences = nilIfEmpty(n.Sequences)
	n.Enums = nilIfEmpty(n.Enums)
}

func normalizeTable(t *Table) {
	t.Comment = strings.TrimSpace(t.Comment)
	t.PartitionKey = collapseSpace(t.PartitionKey)

	for i := range t.Columns {
		normalizeColumn(&t.Columns[i])
	}
	for i := range t.Constraints {
		normalizeConstraint(&t.Constraints[i])
	}
	for i := range t.Indexes {
		normalizeIndex(&t.Indexes[i])
	}

	// Columns sort by name: physical position is an artifact of the order in
	// which columns happened to be added, not a property of the schema.
	sort.Slice(t.Columns, func(i, j int) bool { return t.Columns[i].Name < t.Columns[j].Name })
	sort.Slice(t.Constraints, func(i, j int) bool { return t.Constraints[i].Name < t.Constraints[j].Name })
	sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })

	t.Constraints = nilIfEmpty(t.Constraints)
	t.Indexes = nilIfEmpty(t.Indexes)
}

func normalizeColumn(c *Column) {
	c.Type = collapseSpace(c.Type)
	c.Default = collapseSpace(c.Default)
	c.Generated = collapseSpace(c.Generated)
	c.Identity = strings.ToUpper(strings.TrimSpace(c.Identity))
	c.Comment = strings.TrimSpace(c.Comment)

	// A collation equal to the type's default was not chosen by the author and
	// must not distinguish two otherwise identical schemas.
	if strings.EqualFold(strings.TrimSpace(c.Collation), "default") {
		c.Collation = ""
	} else {
		c.Collation = strings.TrimSpace(c.Collation)
	}
}

func normalizeConstraint(c *Constraint) {
	c.Expression = collapseSpace(c.Expression)
	c.Definition = collapseSpace(c.Definition)
	c.OnDelete = strings.ToUpper(strings.TrimSpace(c.OnDelete))
	c.OnUpdate = strings.ToUpper(strings.TrimSpace(c.OnUpdate))

	// NO ACTION is the engine default and carries no information.
	if c.OnDelete == "NO ACTION" {
		c.OnDelete = ""
	}
	if c.OnUpdate == "NO ACTION" {
		c.OnUpdate = ""
	}

	// Key column order is significant — it determines the order of the backing
	// index — so Columns and RefColumns are never sorted.
	c.Columns = nilIfEmpty(c.Columns)
	c.RefColumns = nilIfEmpty(c.RefColumns)
}

func normalizeIndex(i *Index) {
	i.Method = strings.ToLower(strings.TrimSpace(i.Method))
	i.Predicate = collapseSpace(i.Predicate)
	i.Definition = collapseSpace(i.Definition)
	i.Comment = strings.TrimSpace(i.Comment)

	for j := range i.Columns {
		i.Columns[j] = collapseSpace(i.Columns[j])
	}
	for j := range i.Include {
		i.Include[j] = collapseSpace(i.Include[j])
	}

	// Indexed column order determines which queries the index can serve, so it
	// is preserved. INCLUDE payload order has no such meaning, so it is sorted.
	sort.Strings(i.Include)

	i.Columns = nilIfEmpty(i.Columns)
	i.Include = nilIfEmpty(i.Include)
}

func normalizeSequence(s *Sequence) {
	s.Type = collapseSpace(s.Type)
	s.Comment = strings.TrimSpace(s.Comment)
}

func normalizeEnum(e *Enum) {
	e.Comment = strings.TrimSpace(e.Comment)
	// Label order defines the type's sort order and is never rearranged.
	e.Labels = nilIfEmpty(e.Labels)
}

// collapseSpace trims the string and reduces every internal run of whitespace to
// a single space, so that expressions differing only in formatting compare
// equal.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// nilIfEmpty maps an empty slice to nil so that "absent" and "present but empty"
// cannot produce different serializations of the same schema.
func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}
