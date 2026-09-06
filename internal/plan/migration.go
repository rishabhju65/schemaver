// Package plan holds the artifact a migration is: an ordered list of steps that
// carries a schema version between two exact points.
//
// Nothing here generates a migration — that is the diff engine's job and it does
// not exist yet. These are the types it will produce and the executor consumes.
package plan

import "github.com/rishabhju65/schemaver/internal/schema"

// Step is one executable unit of a migration.
type Step struct {
	// Ordinal is the step's position, from 1.
	Ordinal int

	// SQL is the statement or statements this step executes.
	SQL string

	// Transactional reports whether this step may run inside a transaction.
	// Concurrent index builds may not (D-006), and a migration containing such a
	// step is not atomic — which review has to show rather than let a failure
	// reveal.
	Transactional bool

	// ExpectedAfter is the schema fingerprint once this step has completed.
	//
	// This is what makes recovery deterministic. Recorded progress alone is
	// ambiguous, because a crash between executing a step and recording it
	// leaves no way to know whether it ran. Matching the live fingerprint
	// against these checkpoints identifies the exact position with no reliance
	// on our own bookkeeping.
	ExpectedAfter schema.Version

	// Destructive marks a step that discards data, so it cannot be undone by
	// generated DDL.
	Destructive bool
}

// Migration carries a database from one exact schema version to another.
type Migration struct {
	// Name is for humans. Nothing depends on it, and nothing may order by it
	// (D-007).
	Name string

	// From and To are the migration's identity.
	From schema.Version
	To   schema.Version

	Steps []Step

	// RevertOf names the migration this one undoes, if any.
	RevertOf string

	// IrreversibleReason is set when no revert can be generated. Surfaced at
	// approval time, per D-012 — never discovered during an incident.
	IrreversibleReason string
}

// Reversible reports whether a revert can be generated for this migration.
func (m *Migration) Reversible() bool { return m.IrreversibleReason == "" }

// Chain returns every fingerprint the database passes through, beginning at From
// and ending at To.
//
// A migration whose steps carry no checkpoints still has two known points, so
// the chain is never shorter than From and To. That matters because the chain is
// diagnostic only: safety is decided by the endpoints, and a migration with no
// per-step fingerprints must still be recognisable as started or finished.
func (m *Migration) Chain() []schema.Version {
	out := make([]schema.Version, 0, len(m.Steps)+2)
	out = append(out, m.From)
	for _, s := range m.Steps {
		if s.ExpectedAfter == "" {
			continue
		}
		out = append(out, s.ExpectedAfter)
	}
	if out[len(out)-1] != m.To {
		out = append(out, m.To)
	}
	return out
}

// Checkpointed reports whether every step carries an expected fingerprint. When
// false, the migration can still be executed and reconciled — it simply cannot
// say where it stopped if it fails partway.
func (m *Migration) Checkpointed() bool {
	for _, s := range m.Steps {
		if s.ExpectedAfter == "" {
			return false
		}
	}
	return len(m.Steps) > 0
}

// StepsCompleted reports how many steps must have run for the schema to
// fingerprint as live, and whether live is a recognised point on the chain.
//
// It is meaningful only for a Checkpointed migration; without checkpoints the
// only recognisable points are the endpoints, and the step count it reports
// carries no information.
func (m *Migration) StepsCompleted(live schema.Version) (int, bool) {
	for i, v := range m.Chain() {
		if v == live {
			return i, true
		}
	}
	return 0, false
}
