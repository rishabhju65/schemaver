package diff

import "sort"

// Execution phases.
//
// Ordering is by phase rather than by a dependency graph, and that is a
// deliberate simplification worth stating. The dependencies between DDL
// statements are almost entirely a property of the *kind* of statement — an
// index cannot outlive its column, a foreign key cannot precede the table it
// points at — so a fixed order over kinds is a valid total order consistent
// with those dependencies, and it is far easier to reason about and test than a
// graph rebuilt on every diff.
//
// Teardown runs in reverse dependency order, then buildup in forward order.
// What this does *not* handle is a dependency between two objects of the same
// kind — a view built on another view, say — which is why views are not yet in
// the model.
const (
	phaseDropConstraint = iota + 1
	phaseDropIndex
	phaseDropColumn
	phaseDropTable
	phaseDropType // enums and sequences, after the columns that used them
	phaseDropNamespace

	phaseCreateNamespace
	phaseCreateType // enums and sequences, before the columns that need them
	phaseCreateTable
	phaseAddColumn
	phaseAlterColumn
	// Constraining a column comes after any default or backfill that makes the
	// constraint satisfiable.
	phaseSetNotNull
	phaseAddConstraint
	phaseCreateIndex
	phaseComment
)

func phaseOf(k Kind) int {
	switch k {
	case DropConstraint:
		return phaseDropConstraint
	case DropIndex:
		return phaseDropIndex
	case DropColumn:
		return phaseDropColumn
	case DropTable:
		return phaseDropTable
	case DropEnum, DropSequence:
		return phaseDropType
	case DropNamespace:
		return phaseDropNamespace

	case CreateNamespace:
		return phaseCreateNamespace
	case CreateEnum, AddEnumLabel, AlterEnum, CreateSequence, AlterSequence:
		return phaseCreateType
	case CreateTable:
		return phaseCreateTable
	case AddColumn:
		return phaseAddColumn
	case AlterColumnType, SetDefault, DropDefault, DropNotNull, AlterColumnOther:
		return phaseAlterColumn
	case SetNotNull:
		return phaseSetNotNull
	case AddConstraint:
		return phaseAddConstraint
	case CreateIndex:
		return phaseCreateIndex
	case SetComment:
		return phaseComment
	}
	return phaseComment
}

// Order sorts changes into an executable sequence.
//
// Deterministic: the same diff always yields the same order, so a regenerated
// migration is byte-identical to its predecessor when nothing has changed.
// Within a phase, ordering is by qualified name and then by kind, neither of
// which carries meaning — they exist only to make the result stable.
func Order(changes []Change) []Change {
	out := make([]Change, len(changes))
	copy(out, changes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].phase != out[j].phase {
			return out[i].phase < out[j].phase
		}
		if qi, qj := out[i].Qualified(), out[j].Qualified(); qi != qj {
			return qi < qj
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
