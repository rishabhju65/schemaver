// Package render turns the canonical schema model back into DDL.
//
// It serves two callers with different needs, from one implementation:
//
//   - The UI, which shows a human the DDL behind a table.
//   - Baseline import, which writes the declared schema into the repo.
//
// Where the model carries the engine's own rendering of an object (Definition),
// that is preferred over rebuilding the object structurally. The engine's
// deparser handles operator classes, sort order, opclass parameters and other
// details that are tedious to reconstruct and easy to get subtly wrong.
// Structural reconstruction is the fallback for models that were built by hand
// rather than read from a database.
package render

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rishabhju65/schemaver/internal/schema"
)

var safeIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// reserved lists the keywords that must be quoted even though they look like
// ordinary identifiers. Not exhaustive — it covers the words that actually turn
// up as table and column names.
var reserved = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "authorization": true,
	"binary": true, "both": true, "case": true, "cast": true, "check": true,
	"collate": true, "collation": true, "column": true, "concurrently": true,
	"constraint": true, "create": true, "cross": true, "current_date": true,
	"current_role": true, "current_time": true, "current_timestamp": true,
	"current_user": true, "default": true, "deferrable": true, "desc": true,
	"distinct": true, "do": true, "else": true, "end": true, "except": true,
	"false": true, "fetch": true, "for": true, "foreign": true, "freeze": true,
	"from": true, "full": true, "grant": true, "group": true, "having": true,
	"ilike": true, "in": true, "initially": true, "inner": true, "intersect": true,
	"into": true, "is": true, "isnull": true, "join": true, "lateral": true,
	"leading": true, "left": true, "like": true, "limit": true, "localtime": true,
	"localtimestamp": true, "natural": true, "not": true, "notnull": true,
	"null": true, "offset": true, "on": true, "only": true, "or": true,
	"order": true, "outer": true, "overlaps": true, "placing": true, "primary": true,
	"references": true, "returning": true, "right": true, "select": true,
	"session_user": true, "similar": true, "some": true, "symmetric": true,
	"table": true, "tablesample": true, "then": true, "to": true, "trailing": true,
	"true": true, "union": true, "unique": true, "user": true, "using": true,
	"variadic": true, "verbose": true, "when": true, "where": true, "window": true,
	"with": true,
}

// ident quotes an identifier when the engine would not otherwise read it back
// unchanged.
func ident(s string) string {
	if safeIdent.MatchString(s) && !reserved[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualify(ns, name string) string { return ident(ns) + "." + ident(name) }

// literal renders a string as a SQL literal.
func literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Schema renders the whole schema as DDL, ordered so it can be executed top to
// bottom against an empty database.
//
// The ordering is the point: namespaces, then enums and sequences, then tables,
// then foreign keys once every table exists, then indexes, then comments.
// Foreign keys are deliberately separated from CREATE TABLE so that circular
// references between tables do not make the output unexecutable.
func Schema(s *schema.Schema) string {
	var b strings.Builder

	for _, ns := range s.Namespaces {
		if ns.Name != "public" {
			fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %s;\n\n", ident(ns.Name))
		}
	}
	for _, ns := range s.Namespaces {
		for _, e := range ns.Enums {
			b.WriteString(enumDDL(ns.Name, e))
			b.WriteString("\n\n")
		}
		for _, sq := range ns.Sequences {
			if sq.Owned() {
				// Owned sequences are created by the identity or serial column
				// that owns them; emitting them here would create them twice.
				continue
			}
			b.WriteString(sequenceDDL(ns.Name, sq))
			b.WriteString("\n\n")
		}
	}
	for _, ns := range s.Namespaces {
		for _, t := range ns.Tables {
			b.WriteString(tableDDL(ns.Name, t))
			b.WriteString("\n\n")
		}
	}
	for _, ns := range s.Namespaces {
		for _, t := range ns.Tables {
			for _, c := range t.Constraints {
				if c.Type != schema.ForeignKey {
					continue
				}
				fmt.Fprintf(&b, "ALTER TABLE %s ADD %s;\n", qualify(ns.Name, t.Name), constraintDDL(c))
			}
		}
	}
	if strings.HasSuffix(b.String(), ";\n") {
		b.WriteString("\n")
	}
	for _, ns := range s.Namespaces {
		for _, t := range ns.Tables {
			for _, idx := range t.Indexes {
				b.WriteString(indexDDL(ns.Name, t.Name, idx))
				b.WriteString("\n")
			}
		}
	}
	comments := commentDDL(s)
	if comments != "" {
		b.WriteString("\n")
		b.WriteString(comments)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Table renders one table with its constraints and indexes — what the UI shows
// beside a table in the schema explorer.
func Table(ns string, t schema.Table) string {
	var b strings.Builder
	b.WriteString(tableDDL(ns, t))
	for _, c := range t.Constraints {
		if c.Type == schema.ForeignKey {
			fmt.Fprintf(&b, "\n\nALTER TABLE %s ADD %s;", qualify(ns, t.Name), constraintDDL(c))
		}
	}
	for _, idx := range t.Indexes {
		b.WriteString("\n\n")
		b.WriteString(indexDDL(ns, t.Name, idx))
	}
	return b.String()
}

func tableDDL(ns string, t schema.Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", qualify(ns, t.Name))

	var parts []string
	for _, c := range t.Columns {
		parts = append(parts, "    "+columnDDL(c))
	}
	// Foreign keys are emitted separately so circular references stay
	// executable; every other constraint is inline.
	for _, c := range t.Constraints {
		if c.Type == schema.ForeignKey {
			continue
		}
		parts = append(parts, "    "+constraintDDL(c))
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n)")
	if t.Partitioned && t.PartitionKey != "" {
		fmt.Fprintf(&b, " PARTITION BY %s", t.PartitionKey)
	}
	b.WriteString(";")
	return b.String()
}

func columnDDL(c schema.Column) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", ident(c.Name), c.Type)
	if c.Collation != "" {
		fmt.Fprintf(&b, " COLLATE %s", ident(c.Collation))
	}
	switch {
	case c.Generated != "":
		fmt.Fprintf(&b, " GENERATED ALWAYS AS (%s) STORED", c.Generated)
	case c.Identity != "":
		fmt.Fprintf(&b, " GENERATED %s AS IDENTITY", c.Identity)
	}
	if !c.Nullable {
		b.WriteString(" NOT NULL")
	}
	// A generated column carries its expression above; it cannot also have a
	// default.
	if c.Default != "" && c.Generated == "" {
		fmt.Fprintf(&b, " DEFAULT %s", c.Default)
	}
	return b.String()
}

func constraintDDL(c schema.Constraint) string {
	if c.Definition != "" {
		return fmt.Sprintf("CONSTRAINT %s %s", ident(c.Name), c.Definition)
	}

	var body string
	switch c.Type {
	case schema.PrimaryKey:
		body = fmt.Sprintf("PRIMARY KEY (%s)", identList(c.Columns))
	case schema.Unique:
		body = fmt.Sprintf("UNIQUE (%s)", identList(c.Columns))
	case schema.Check:
		body = fmt.Sprintf("CHECK (%s)", c.Expression)
	case schema.ForeignKey:
		ref := ident(c.RefTable)
		if c.RefNamespace != "" {
			ref = qualify(c.RefNamespace, c.RefTable)
		}
		body = fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)",
			identList(c.Columns), ref, identList(c.RefColumns))
		if c.OnDelete != "" {
			body += " ON DELETE " + c.OnDelete
		}
		if c.OnUpdate != "" {
			body += " ON UPDATE " + c.OnUpdate
		}
	case schema.Exclusion:
		// An exclusion constraint cannot be rebuilt from the fields the model
		// carries; without the engine's rendering there is nothing to emit.
		body = "EXCLUDE /* unrenderable: original definition not available */"
	}
	if c.Deferrable {
		body += " DEFERRABLE"
		if c.InitiallyDeferred {
			body += " INITIALLY DEFERRED"
		}
	}
	return fmt.Sprintf("CONSTRAINT %s %s", ident(c.Name), body)
}

func indexDDL(ns, table string, i schema.Index) string {
	if i.Definition != "" {
		return i.Definition + ";"
	}
	unique := ""
	if i.Unique {
		unique = "UNIQUE "
	}
	method := i.Method
	if method == "" {
		method = "btree"
	}
	out := fmt.Sprintf("CREATE %sINDEX %s ON %s USING %s (%s)",
		unique, ident(i.Name), qualify(ns, table), method, strings.Join(i.Columns, ", "))
	if len(i.Include) > 0 {
		out += fmt.Sprintf(" INCLUDE (%s)", identList(i.Include))
	}
	if i.Predicate != "" {
		out += fmt.Sprintf(" WHERE %s", i.Predicate)
	}
	return out + ";"
}

func enumDDL(ns string, e schema.Enum) string {
	labels := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		labels[i] = literal(l)
	}
	return fmt.Sprintf("CREATE TYPE %s AS ENUM (%s);", qualify(ns, e.Name), strings.Join(labels, ", "))
}

func sequenceDDL(ns string, s schema.Sequence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE SEQUENCE %s", qualify(ns, s.Name))
	if s.Type != "" && s.Type != "bigint" {
		fmt.Fprintf(&b, " AS %s", s.Type)
	}
	fmt.Fprintf(&b, " START WITH %d INCREMENT BY %d", s.Start, s.Increment)
	fmt.Fprintf(&b, " MINVALUE %d MAXVALUE %d CACHE %d", s.Min, s.Max, s.Cache)
	if s.Cycle {
		b.WriteString(" CYCLE")
	}
	b.WriteString(";")
	return b.String()
}

func commentDDL(s *schema.Schema) string {
	var lines []string
	for _, ns := range s.Namespaces {
		if ns.Comment != "" {
			lines = append(lines, fmt.Sprintf("COMMENT ON SCHEMA %s IS %s;", ident(ns.Name), literal(ns.Comment)))
		}
		for _, t := range ns.Tables {
			if t.Comment != "" {
				lines = append(lines, fmt.Sprintf("COMMENT ON TABLE %s IS %s;", qualify(ns.Name, t.Name), literal(t.Comment)))
			}
			for _, c := range t.Columns {
				if c.Comment != "" {
					lines = append(lines, fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s;",
						qualify(ns.Name, t.Name), ident(c.Name), literal(c.Comment)))
				}
			}
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func identList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ident(n)
	}
	return strings.Join(out, ", ")
}
