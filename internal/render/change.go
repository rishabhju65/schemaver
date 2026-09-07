package render

import (
	"fmt"
	"strings"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Statement is one executable step of a migration.
type Statement struct {
	// SQL is the statement to run.
	SQL string `json:"sql"`

	// ChangeID names the semantic change this implements, so a failure can be
	// attributed to a change rather than to a line number.
	ChangeID string `json:"change_id"`

	// Transactional reports whether this statement may run inside a
	// transaction. A concurrent index build may not, and a migration containing
	// one is therefore not atomic — which review has to show rather than let a
	// failure reveal (D-006).
	Transactional bool `json:"transactional"`

	// Note carries a caveat a reviewer should read: a cast that may not be
	// implicit, a construct we could not fully render.
	Note string `json:"note,omitempty"`
}

// Statements turns a change set into executable SQL, in the order given.
//
// Both schemas are needed, not just the change list: a change says *that* a
// column was added, and the definition of what to add lives in the target
// schema. Rendering from the change alone would mean duplicating every object's
// details into the change, which is how the two drift apart.
//
// Where a statement cannot be rendered with certainty — a type change whose cast
// may not be implicit — the simple form is emitted with a note rather than a
// guess. The shadow proof (D-009) then catches it before any real database is
// touched, which is the system working as intended rather than a gap in it.
func Statements(changes []diff.Change, from, to *schema.Schema) []Statement {
	before, after := index(from), index(to)

	var out []Statement
	for _, c := range changes {
		out = append(out, statementsFor(c, before, after)...)
	}
	return out
}

// objects is a schema flattened for lookup by qualified name.
type objects struct {
	namespaces map[string]schema.Namespace
	tables     map[string]schema.Table
	enums      map[string]schema.Enum
	sequences  map[string]schema.Sequence
	columns    map[string]schema.Column
	constraint map[string]schema.Constraint
	indexes    map[string]schema.Index
}

func index(s *schema.Schema) objects {
	o := objects{
		namespaces: map[string]schema.Namespace{}, tables: map[string]schema.Table{},
		enums: map[string]schema.Enum{}, sequences: map[string]schema.Sequence{},
		columns: map[string]schema.Column{}, constraint: map[string]schema.Constraint{},
		indexes: map[string]schema.Index{},
	}
	if s == nil {
		return o
	}
	for _, ns := range s.Namespaces {
		o.namespaces[ns.Name] = ns
		for _, e := range ns.Enums {
			o.enums[ns.Name+"."+e.Name] = e
		}
		for _, sq := range ns.Sequences {
			o.sequences[ns.Name+"."+sq.Name] = sq
		}
		for _, t := range ns.Tables {
			key := ns.Name + "." + t.Name
			o.tables[key] = t
			for _, c := range t.Columns {
				o.columns[key+"."+c.Name] = c
			}
			for _, c := range t.Constraints {
				o.constraint[key+"."+c.Name] = c
			}
			for _, i := range t.Indexes {
				o.indexes[key+"."+i.Name] = i
			}
		}
	}
	return o
}

func statementsFor(c diff.Change, before, after objects) []Statement {
	one := func(sql string) []Statement {
		return []Statement{{SQL: sql, ChangeID: c.ID, Transactional: true}}
	}
	table := qualify(c.Namespace, c.Table)
	object := c.Namespace + "." + c.Table + "." + c.Object

	switch c.Kind {
	case diff.CreateNamespace:
		return one(fmt.Sprintf("CREATE SCHEMA %s;", ident(c.Namespace)))
	case diff.DropNamespace:
		// CASCADE, because a namespace being dropped has already had its
		// contents reported as separate destructive changes; refusing here
		// would stall on objects the reviewer has already agreed to lose.
		return one(fmt.Sprintf("DROP SCHEMA %s CASCADE;", ident(c.Namespace)))

	case diff.CreateEnum:
		e, ok := after.enums[c.Namespace+"."+c.Object]
		if !ok {
			return unrenderable(c, "the type is not present in the target schema")
		}
		return one(enumDDL(c.Namespace, e))
	case diff.DropEnum:
		return one(fmt.Sprintf("DROP TYPE %s;", qualify(c.Namespace, c.Object)))
	case diff.AddEnumLabel:
		s := one(fmt.Sprintf("ALTER TYPE %s ADD VALUE %s;",
			qualify(c.Namespace, c.Object), literal(c.To)))
		// PostgreSQL permits this inside a transaction, but the new value
		// cannot be *used* in the same one — so anything depending on it must
		// be a later statement.
		s[0].Note = "the new value cannot be used until this statement's transaction commits"
		return s
	case diff.AlterEnum:
		return unrenderable(c,
			"reordering or removing enum values requires recreating the type and "+
				"rewriting every column that uses it")

	case diff.CreateSequence:
		sq, ok := after.sequences[c.Namespace+"."+c.Object]
		if !ok {
			return unrenderable(c, "the sequence is not present in the target schema")
		}
		return one(sequenceDDL(c.Namespace, sq))
	case diff.DropSequence:
		return one(fmt.Sprintf("DROP SEQUENCE %s;", qualify(c.Namespace, c.Object)))
	case diff.AlterSequence:
		sq, ok := after.sequences[c.Namespace+"."+c.Object]
		if !ok {
			return unrenderable(c, "the sequence is not present in the target schema")
		}
		return one(fmt.Sprintf("ALTER SEQUENCE %s INCREMENT BY %d MINVALUE %d MAXVALUE %d CACHE %d%s;",
			qualify(c.Namespace, sq.Name), sq.Increment, sq.Min, sq.Max, sq.Cache,
			cycleClause(sq.Cycle)))

	case diff.CreateTable:
		t, ok := after.tables[c.Namespace+"."+c.Table]
		if !ok {
			return unrenderable(c, "the table is not present in the target schema")
		}
		return one(tableDDL(c.Namespace, t))
	case diff.DropTable:
		return one(fmt.Sprintf("DROP TABLE %s;", table))

	case diff.AddColumn:
		col, ok := after.columns[object]
		if !ok {
			return unrenderable(c, "the column is not present in the target schema")
		}
		return one(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", table, columnDDL(col)))
	case diff.DropColumn:
		return one(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", table, ident(c.Object)))

	case diff.AlterColumnType:
		s := one(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s;",
			table, ident(c.Object), c.To))
		// Emitted without a USING clause because whether the cast is implicit
		// depends on the type pair and any custom casts installed. If it is not,
		// this fails in the shadow database rather than in production, and a
		// USING clause has to be supplied by hand.
		s[0].Note = "no USING clause: if the cast is not implicit this will fail the shadow proof"
		return s
	case diff.SetNotNull:
		return one(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;",
			table, ident(c.Object)))
	case diff.DropNotNull:
		return one(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;",
			table, ident(c.Object)))
	case diff.SetDefault:
		return one(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;",
			table, ident(c.Object), c.To))
	case diff.DropDefault:
		return one(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;",
			table, ident(c.Object)))
	case diff.AlterColumnOther:
		return unrenderable(c,
			"changing how a column is generated, identified or collated is not "+
				"expressible as one statement and needs authoring by hand")

	case diff.AddConstraint:
		con, ok := after.constraint[object]
		if !ok {
			return unrenderable(c, "the constraint is not present in the target schema")
		}
		return one(fmt.Sprintf("ALTER TABLE %s ADD %s;", table, constraintDDL(con)))
	case diff.DropConstraint:
		return one(fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;", table, ident(c.Object)))

	case diff.CreateIndex:
		idx, ok := after.indexes[object]
		if !ok {
			return unrenderable(c, "the index is not present in the target schema")
		}
		// Built concurrently only on a table that already had rows to lock. A
		// brand-new table has none, so the concurrent form buys nothing and
		// costs atomicity.
		_, existed := before.tables[c.Namespace+"."+c.Table]
		if !existed {
			return one(indexDDL(c.Namespace, c.Table, idx))
		}
		return []Statement{{
			SQL:      concurrentIndexDDL(c.Namespace, c.Table, idx),
			ChangeID: c.ID, Transactional: false,
			Note: "built concurrently, so it cannot run in a transaction; a failure " +
				"leaves an invalid index that must be dropped before retrying",
		}}
	case diff.DropIndex:
		return []Statement{{
			SQL: fmt.Sprintf("DROP INDEX CONCURRENTLY %s;",
				qualify(c.Namespace, c.Object)),
			ChangeID: c.ID, Transactional: false,
		}}

	case diff.SetComment:
		return one(commentStatement(c))
	}
	return unrenderable(c, fmt.Sprintf("no renderer for change kind %q", c.Kind))
}

// unrenderable reports a change the renderer cannot express, as a comment
// carrying the reason.
//
// Emitted rather than skipped: a migration missing a step would pass review and
// then fail its own shadow proof with no indication why. A visible placeholder
// fails loudly and tells the author what to write.
func unrenderable(c diff.Change, why string) []Statement {
	return []Statement{{
		SQL:      fmt.Sprintf("-- MANUAL: %s\n--   %s", c.Summary, why),
		ChangeID: c.ID, Transactional: true,
		Note: "cannot be generated: " + why,
	}}
}

func commentStatement(c diff.Change) string {
	body := literal(c.To)
	if c.To == "" {
		body = "NULL"
	}
	switch {
	case c.Object != "" && c.Table != "":
		return fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s;",
			qualify(c.Namespace, c.Table), ident(c.Object), body)
	case c.Table != "":
		return fmt.Sprintf("COMMENT ON TABLE %s IS %s;", qualify(c.Namespace, c.Table), body)
	default:
		return fmt.Sprintf("COMMENT ON SCHEMA %s IS %s;", ident(c.Namespace), body)
	}
}

func cycleClause(cycle bool) string {
	if cycle {
		return " CYCLE"
	}
	return " NO CYCLE"
}

// concurrentIndexDDL renders an index build that does not block writes.
func concurrentIndexDDL(ns, table string, i schema.Index) string {
	sql := indexDDL(ns, table, i)
	if strings.HasPrefix(sql, "CREATE UNIQUE INDEX ") {
		return strings.Replace(sql, "CREATE UNIQUE INDEX ", "CREATE UNIQUE INDEX CONCURRENTLY ", 1)
	}
	return strings.Replace(sql, "CREATE INDEX ", "CREATE INDEX CONCURRENTLY ", 1)
}

// SQL renders a statement list as a migration script, with transaction
// boundaries marked where atomicity ends.
func SQL(statements []Statement) string {
	var b strings.Builder
	for i, s := range statements {
		if !s.Transactional {
			b.WriteString("-- not transactional: this statement runs on its own\n")
		}
		if s.Note != "" {
			fmt.Fprintf(&b, "-- %s\n", s.Note)
		}
		b.WriteString(s.SQL)
		if !strings.HasSuffix(s.SQL, ";") && !strings.HasPrefix(s.SQL, "--") {
			b.WriteString(";")
		}
		b.WriteString("\n")
		if i < len(statements)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
