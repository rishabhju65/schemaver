package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ErrNoCommonAncestor is returned when two databases have never been observed
// at the same schema.
//
// Not a failure so much as an absence: without a point they both came from,
// there is no way to tell what each did independently from what one of them
// never had. A comparison is still possible and is what the product did before
// merging existed — it just cannot claim to preserve anything.
var ErrNoCommonAncestor = errors.New(
	"these two databases have never been seen at the same schema, so there is " +
		"nothing to measure their divergence from")

// MergeBase is the most recent schema both databases have been observed at.
//
// Their last agreement, recovered from the snapshots each has left behind. It
// is what makes a merge possible rather than an overwrite: with it, each side's
// independent work is the difference from here, and the two can be compared to
// each other rather than one being imposed on the other.
//
// Recency is measured by the later of the two sightings, so the base is the
// last moment both were there rather than the last time either one was.
func (s *Scope) MergeBase(ctx context.Context, a, b int64) (schema.Version, error) {
	var fingerprint string
	err := s.store.pool.QueryRow(ctx, `
		WITH seen AS (
			SELECT sn.database_id, sn.fingerprint, max(sn.observed_at) AS at
			  FROM schemaver.snapshot sn
			  JOIN schemaver.database d ON d.id = sn.database_id
			  JOIN schemaver.instance i ON i.id = d.instance_id
			 WHERE sn.database_id IN ($1, $2) AND sn.fingerprint IS NOT NULL
			   AND i.project_id = ANY($3)
			 GROUP BY sn.database_id, sn.fingerprint
		)
		SELECT x.fingerprint
		  FROM seen x JOIN seen y ON y.fingerprint = x.fingerprint
		 WHERE x.database_id = $1 AND y.database_id = $2
		 ORDER BY least(x.at, y.at) DESC
		 LIMIT 1`, a, b, s.projects).Scan(&fingerprint)
	if err != nil {
		return "", ErrNoCommonAncestor
	}
	return schema.Version(fingerprint), nil
}

// MergeView is a proposed merge, as a reviewer should read it.
type MergeView struct {
	Base schema.Version
	// Ours is what the database being changed did since the base; Theirs is
	// what the source did. Both are for reading: they are how a reviewer sees
	// that this is a merge and what each side brought to it.
	Ours   []diff.Change
	Theirs []diff.Change

	// Result is the merged schema, and Target its fingerprint. This — not the
	// source's schema — is what the migration declares and what the shadow
	// proof checks the rehearsal against, because applying both sides' work
	// lands on neither side's current schema.
	Result *schema.Schema
	Target schema.Version

	// Apply is the difference between where the target database is now and the
	// merged result: the work the migration actually has to do.
	Apply     []diff.Change
	Conflicts []diff.Conflict
}

// Clean reports a merge with nothing to resolve by hand.
func (m *MergeView) Clean() bool { return len(m.Conflicts) == 0 }

// Diverged reports that both sides moved since the base, which is when the
// difference between a merge and an overwrite starts to matter.
func (m *MergeView) Diverged() bool { return len(m.Ours) > 0 && len(m.Theirs) > 0 }

// PlanMerge works out what bringing one database in line with another should
// actually do, given where the two last agreed.
func (s *Scope) PlanMerge(ctx context.Context, target, source int64) (*MergeView, error) {
	base, err := s.MergeBase(ctx, target, source)
	if err != nil {
		return nil, err
	}
	baseSchema, err := s.Blob(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("read the common ancestor: %w", err)
	}

	current := func(id int64) (*schema.Schema, error) {
		var fingerprint string
		if err := s.store.pool.QueryRow(ctx, `
			SELECT COALESCE(d.current_fingerprint, '')
			  FROM schemaver.database d
			  JOIN schemaver.instance i ON i.id = d.instance_id
			 WHERE d.id = $1 AND i.project_id = ANY($2)`, id, s.projects).
			Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("read database %d: %w", id, err)
		}
		if fingerprint == "" {
			return nil, fmt.Errorf("database %d has not been read yet", id)
		}
		return s.Blob(ctx, schema.Version(fingerprint))
	}

	ourSchema, err := current(target)
	if err != nil {
		return nil, err
	}
	theirSchema, err := current(source)
	if err != nil {
		return nil, err
	}

	view := &MergeView{
		Base:   base,
		Ours:   diff.Compute(baseSchema, ourSchema).Changes,
		Theirs: diff.Compute(baseSchema, theirSchema).Changes,
	}

	m := diff.ThreeWay(baseSchema, ourSchema, theirSchema)
	if !m.Clean() {
		view.Conflicts = m.Conflicts
		return view, nil
	}

	fingerprint, err := schema.Fingerprint(m.Schema)
	if err != nil {
		return nil, fmt.Errorf("fingerprint the merged schema: %w", err)
	}
	view.Result, view.Target = m.Schema, fingerprint
	view.Apply = diff.Compute(ourSchema, m.Schema).Changes
	return view, nil
}

// putBlob stores a schema that no database has been observed at.
//
// Every other schema in the system arrives by being read off a real database,
// and is stored on the way in. A merged schema is the exception: it is
// computed, and it has to exist as a blob before a migration can declare it as
// its target, because that column is a foreign key into the blobs.
//
// Content-addressed like the rest, so a merge computed twice is stored once.
func (s *Store) putBlob(ctx context.Context, fingerprint schema.Version, sch *schema.Schema) error {
	canonical, err := schema.Canonical(sch)
	if err != nil {
		return fmt.Errorf("render the merged schema: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, tableCount(sch), len(canonical)); err != nil {
		return fmt.Errorf("store the merged schema: %w", err)
	}
	return nil
}
