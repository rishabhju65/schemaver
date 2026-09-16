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
	// First, before anything else at all. Every other change to a renamed table
	// names it by the name it will have — the diff compares the old table
	// against the new one under the new name — so the table has to be called
	// that before any of them run. Dropping a constraint from a table whose
	// rename has not happened yet addresses a table that does not exist.
	//
	// Nothing needs to precede it. A rename into a name another table still
	// holds would, but that is refused rather than ordered around: see
	// tableChanges.
	phaseRenameTable = iota + 1

	phaseDropConstraint
	phaseDropIndex
	phaseDropColumn
	phaseDropTable
	phaseDropType // enums and sequences, after the columns that used them
	phaseDropNamespace

	phaseCreateNamespace
	phaseCreateType // enums and sequences, before the columns that need them
	phaseCreateTable
	// Before columns are added, and after they are dropped. Both orderings
	// matter: renaming a to b while adding a new a would rename the wrong
	// column if the add came first, and renaming a to b while dropping the old
	// b would collide if the drop came second.
	phaseRenameColumn
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
	case RenameTable:
		return phaseRenameTable
	case CreateTable:
		return phaseCreateTable
	case RenameColumn:
		return phaseRenameColumn
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
