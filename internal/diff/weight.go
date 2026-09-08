package diff

// Scheduling weights.
//
// Connections are not the binding constraint on a migration: two concurrent
// index builds compete for the same buffer cache and write-ahead log however
// many connection slots are free. So the cost of running something is derived
// from what it does, using the classification already computed for review.
const (
	// WeightTrivial is metadata-only or additive work — a catalogue change that
	// touches no rows. Many can run at once without interfering.
	WeightTrivial = 1
	// WeightLockHeavy is an index build or constraint validation: it scans the
	// table and competes for IO.
	WeightLockHeavy = 4
	// WeightRewriting is a table rewrite or a destructive change, which
	// effectively claims the instance for its duration.
	WeightRewriting = 8
	// WeightUnknown is what an unclassified change costs. The expensive
	// assumption is the safe one — understating cost is how a migration takes an
	// instance down alongside four others.
	WeightUnknown = WeightRewriting
)

// Weight reports what executing this change set should cost against an
// instance's budget.
//
// The heaviest change decides it, not the sum. A migration adding twelve
// nullable columns is still trivial; one adding a column and rewriting a table
// costs what the rewrite costs, because the rewrite is what saturates the disk.
func Weight(changes []Change) int {
	weight := WeightTrivial
	for _, c := range changes {
		if w := weightOf(c.Class); w > weight {
			weight = w
		}
	}
	return weight
}

func weightOf(c Class) int {
	switch c {
	case MetadataOnly, Additive:
		return WeightTrivial
	case LockHeavy:
		return WeightLockHeavy
	case Rewriting, Destructive:
		return WeightRewriting
	}
	return WeightUnknown
}
