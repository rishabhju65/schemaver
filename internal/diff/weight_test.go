package diff

import "testing"

func TestWeightFollowsTheHeaviestChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		classes []Class
		want    int
	}{
		{"nothing", nil, WeightTrivial},
		{"a dozen additive columns", []Class{Additive, Additive, Additive,
			Additive, Additive, Additive, Additive, Additive, Additive,
			Additive, Additive, Additive}, WeightTrivial},
		{"comments only", []Class{MetadataOnly}, WeightTrivial},
		{"an index build", []Class{Additive, LockHeavy}, WeightLockHeavy},
		{"a rewrite among trivia", []Class{MetadataOnly, Additive, Rewriting}, WeightRewriting},
		{"a drop", []Class{Destructive}, WeightRewriting},
		{"an unrecognised class", []Class{Class("something new")}, WeightUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes := make([]Change, len(tc.classes))
			for i, c := range tc.classes {
				changes[i] = Change{Class: c}
			}
			if got := Weight(changes); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWeightDoesNotAccumulate is the property that keeps a large but harmless
// migration schedulable. Summing would make twelve added columns cost more than
// a table rewrite, which is backwards.
func TestWeightDoesNotAccumulate(t *testing.T) {
	many := make([]Change, 40)
	for i := range many {
		many[i] = Change{Class: Additive}
	}
	if got := Weight(many); got != WeightTrivial {
		t.Errorf("forty additive changes weigh %d; they should still be trivial", got)
	}

	one := []Change{{Class: Rewriting}}
	if Weight(many) >= Weight(one) {
		t.Error("many trivial changes outweigh a single rewrite")
	}
}

// TestUnknownIsExpensive guards the direction of the default. A new class
// defaulting to cheap would let it run alongside everything else.
func TestUnknownIsExpensive(t *testing.T) {
	if WeightUnknown != WeightRewriting {
		t.Error("an unclassified change should cost as much as the most expensive known one")
	}
}
