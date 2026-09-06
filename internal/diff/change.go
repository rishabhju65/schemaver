// Package diff computes what differs between two schemas, as typed changes
// rather than as text.
//
// This is what separates schemaver from a tool that diffs .sql files. A text
// patch can say a line changed; it cannot say that a column was dropped, that
// the drop destroys data, that an index must go first, or that this change and
// that one are the same change seen twice. Everything downstream — migration
// generation, revert generation, safety rules, review comments — reads these
// types and never the SQL.
package diff

import (
	"fmt"
	"strings"
)

// Kind is what a change does.
type Kind string

const (
	CreateNamespace Kind = "create_namespace"
	DropNamespace   Kind = "drop_namespace"

	CreateEnum   Kind = "create_enum"
	DropEnum     Kind = "drop_enum"
	AddEnumLabel Kind = "add_enum_label"
	AlterEnum    Kind = "alter_enum"

	CreateSequence Kind = "create_sequence"
	DropSequence   Kind = "drop_sequence"
	AlterSequence  Kind = "alter_sequence"

	CreateTable Kind = "create_table"
	DropTable   Kind = "drop_table"

	AddColumn        Kind = "add_column"
	DropColumn       Kind = "drop_column"
	AlterColumnType  Kind = "alter_column_type"
	SetNotNull       Kind = "set_not_null"
	DropNotNull      Kind = "drop_not_null"
	SetDefault       Kind = "set_default"
	DropDefault      Kind = "drop_default"
	AlterColumnOther Kind = "alter_column_other"

	AddConstraint  Kind = "add_constraint"
	DropConstraint Kind = "drop_constraint"

	CreateIndex Kind = "create_index"
	DropIndex   Kind = "drop_index"

	SetComment Kind = "set_comment"
)

// Class is what a change costs.
//
// This is the axis review actually cares about. "A column changed" is not
// useful; "this rewrites a two hundred million row table while holding an
// exclusive lock" is.
type Class string

const (
	// Additive adds something without touching existing rows.
	Additive Class = "additive"
	// Destructive discards data that no generated DDL can restore.
	Destructive Class = "destructive"
	// Rewriting rewrites the table, so its cost scales with table size.
	Rewriting Class = "rewriting"
	// LockHeavy takes a lock that blocks other work for as long as it runs,
	// typically because it must scan the whole table to verify something.
	LockHeavy Class = "lock_heavy"
	// MetadataOnly touches the catalog and nothing else.
	MetadataOnly Class = "metadata_only"
)

// Blocks reports whether a change prevents concurrent access while it runs.
func (c Class) Blocks() bool { return c == Rewriting || c == LockHeavy }

// Change is one difference between two schemas.
type Change struct {
	// ID identifies this change semantically, not positionally.
	//
	// It is derived from what the change *is* — "drop the column
	// public.orders.legacy_status" — so it survives the migration being
	// regenerated against a new base, which D-007 requires and which comment
	// anchoring (D-008) depends on absolutely. An id derived from a line number
	// or a hash of the SQL would be orphaned by every regeneration.
	ID string `json:"id"`

	Kind  Kind  `json:"kind"`
	Class Class `json:"class"`

	Namespace string `json:"namespace,omitempty"`
	Table     string `json:"table,omitempty"`
	Object    string `json:"object,omitempty"`

	// Summary is how this change reads to a person.
	Summary string `json:"summary"`

	// Detail carries the before and after where one exists.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// phase orders execution; see order.go.
	phase int
	// typeHint carries a column's type for changes where it is not otherwise
	// recorded, so rename detection can compare types without parsing prose.
	typeHint string
}

// Qualified returns the dotted name of whatever this change targets.
func (c Change) Qualified() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{c.Namespace, c.Table, c.Object} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

// newChange builds a change with its identity and class derived from its kind
// and target.
func newChange(kind Kind, ns, table, object, summary string) Change {
	c := Change{
		Kind: kind, Namespace: ns, Table: table, Object: object,
		Summary: summary, Class: classOf(kind), phase: phaseOf(kind),
	}
	c.ID = fmt.Sprintf("%s:%s", kind, c.Qualified())
	return c
}

// classOf assigns the cost class of a change kind.
//
// The assignments assume a modern PostgreSQL. Adding a column with a constant
// default has been metadata-only since PostgreSQL 11; on older versions it
// rewrote the table, and a tool that assumed otherwise would badly understate
// the cost.
func classOf(k Kind) Class {
	switch k {
	case DropTable, DropColumn, DropEnum, DropSequence, DropNamespace:
		return Destructive

	case AlterColumnType:
		// Some type changes are metadata-only (widening varchar, for instance)
		// but most rewrite. Without table statistics the honest default is the
		// expensive one; understating cost is the dangerous direction.
		return Rewriting

	case SetNotNull, AddConstraint:
		// Both must verify every existing row before they can be trusted.
		return LockHeavy

	case CreateIndex:
		// Blocking unless built concurrently, which the planner decides later.
		return LockHeavy

	case CreateTable, CreateNamespace, CreateEnum, CreateSequence,
		AddColumn, AddEnumLabel:
		return Additive

	case DropConstraint, DropIndex, DropNotNull, SetDefault, DropDefault,
		SetComment, AlterSequence, AlterEnum, AlterColumnOther:
		return MetadataOnly
	}
	return MetadataOnly
}

// Summary counts a change set by class.
type Summary struct {
	Total        int `json:"total"`
	Additive     int `json:"additive"`
	Destructive  int `json:"destructive"`
	Rewriting    int `json:"rewriting"`
	LockHeavy    int `json:"lock_heavy"`
	MetadataOnly int `json:"metadata_only"`
}

// Safe reports whether the change set touches nothing irreversible and blocks
// nothing.
func (s Summary) Safe() bool {
	return s.Destructive == 0 && s.Rewriting == 0 && s.LockHeavy == 0
}

// Count summarises a change set.
func Count(changes []Change) Summary {
	s := Summary{Total: len(changes)}
	for _, c := range changes {
		switch c.Class {
		case Additive:
			s.Additive++
		case Destructive:
			s.Destructive++
		case Rewriting:
			s.Rewriting++
		case LockHeavy:
			s.LockHeavy++
		case MetadataOnly:
			s.MetadataOnly++
		}
	}
	return s
}
