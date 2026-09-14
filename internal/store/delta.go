package store

import (
	"context"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/history"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Delta is the difference between two schemas, at both the granularities a
// reader wants it.
//
// Objects first, because that is the question somebody asks before any other:
// what is this change to — which tables, which types. Then the operations,
// which are what will actually run. A list of fourteen statements answers the
// second question well and the first one not at all, and the first is the one
// that decides whether to keep reading.
type Delta struct {
	// Objects is which tables, enums and sequences differ, and Summary counts
	// them.
	Objects []history.ObjectChange
	Summary history.Summary

	// Changes is what differs inside them, classified by cost.
	Changes []diff.Change
}

// Empty reports a delta with nothing in it.
func (d Delta) Empty() bool { return len(d.Changes) == 0 && len(d.Objects) == 0 }

// ObjectsIn returns the changes that fall inside one object, so a reader who
// has picked a table out of the summary can see what happened to it without
// reading the rest.
func (d Delta) ObjectsIn(name string) []diff.Change {
	var out []diff.Change
	for _, c := range d.Changes {
		if c.Namespace+"."+c.Table == name || c.Namespace+"."+c.Object == name {
			out = append(out, c)
		}
	}
	return out
}

// Between reads two stored schemas and reports how they differ.
//
// One place, because the branch page, the review page and the drift page were
// all going to want the same two granularities of the same comparison, and
// three copies of it would have been three chances for them to disagree about
// what a change is.
func (s *Scope) Between(ctx context.Context, from, to schema.Version) (Delta, error) {
	if from == "" || to == "" {
		return Delta{}, fmt.Errorf("a comparison needs both schemas")
	}
	before, err := s.Blob(ctx, from)
	if err != nil {
		return Delta{}, fmt.Errorf("read %s: %w", from.Short(), err)
	}
	after, err := s.Blob(ctx, to)
	if err != nil {
		return Delta{}, fmt.Errorf("read %s: %w", to.Short(), err)
	}
	objects := history.ObjectsChanged(before, after)
	return Delta{
		Objects: objects,
		Summary: history.Count(objects),
		Changes: diff.Compute(before, after).Changes,
	}, nil
}
