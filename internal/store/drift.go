package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/drift"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Expectation resolves what a database is supposed to look like.
//
// Returns nil when nothing is configured to compare against. That is a
// legitimate state — the database is still observed and explorable — and is
// reported as "not comparable" rather than "no drift", because the two look
// identical on a dashboard and mean opposite things.
func (s *Store) Expectation(ctx context.Context, databaseID int64) (*drift.Expectation, error) {
	var repositoryID, peerID *int64
	err := s.pool.QueryRow(ctx,
		`SELECT repository_id, expected_peer_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&repositoryID, &peerID)
	if err != nil {
		return nil, fmt.Errorf("load expectation for database %d: %w", databaseID, err)
	}

	switch {
	case repositoryID != nil:
		var fingerprint, ref, name string
		err := s.pool.QueryRow(ctx, `
			SELECT d.fingerprint, d.git_ref, r.name
			  FROM schemaver.declared_schema d
			  JOIN schemaver.repository r ON r.id = d.repository_id
			 WHERE d.repository_id = $1
			 ORDER BY d.imported_at DESC
			 LIMIT 1`, *repositoryID).Scan(&fingerprint, &ref, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			// Linked to a repository that has never been imported: comparable in
			// principle, not yet in practice.
			return &drift.Expectation{
				Source: drift.FromDeclared,
				Label:  fmt.Sprintf("declared schema in %s", name),
			}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load declared schema: %w", err)
		}
		return &drift.Expectation{
			Source:      drift.FromDeclared,
			Fingerprint: schema.Version(fingerprint),
			Label:       fmt.Sprintf("declared schema in %s at %s", name, ref),
		}, nil

	case peerID != nil:
		var fingerprint *string
		var name, instance string
		err := s.pool.QueryRow(ctx, `
			SELECT d.current_fingerprint, d.name, i.name
			  FROM schemaver.database d
			  JOIN schemaver.instance i ON i.id = d.instance_id
			 WHERE d.id = $1`, *peerID).Scan(&fingerprint, &name, &instance)
		if err != nil {
			return nil, fmt.Errorf("load peer database %d: %w", *peerID, err)
		}
		exp := &drift.Expectation{
			Source:         drift.FromPeer,
			PeerDatabaseID: *peerID,
			Label:          fmt.Sprintf("%s on %s", name, instance),
		}
		if fingerprint != nil {
			exp.Fingerprint = schema.Version(*fingerprint)
		}
		return exp, nil
	}
	return nil, nil
}

// RecordDrift persists the outcome of a comparison.
//
// Drift is state, not an event stream: a divergence that persists for a week is
// one row with a moving last_seen, not one row per check. Anything else trains
// people to ignore the alert, which is the precise failure drift detection
// exists to prevent.
//
// Returns whether a new divergence was opened and how many stale ones were
// closed.
func (s *Store) RecordDrift(ctx context.Context, databaseID int64, r drift.Result) (opened bool, resolved int, err error) {
	if !r.Comparable {
		// Nothing to compare against. Existing open rows are left alone: losing
		// the expectation does not mean the divergence went away.
		return false, 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Close anything that no longer describes reality — either the database came
	// back into agreement, or it moved to a different divergence.
	tag, err := tx.Exec(ctx, `
		UPDATE schemaver.drift
		   SET status = 'resolved', resolved_at = now()
		 WHERE database_id = $1 AND status = 'open'
		   AND NOT ($2::boolean
		            AND observed_fingerprint = $3 AND expected_fingerprint = $4)`,
		databaseID, r.Drifted, string(r.Observed), string(r.Expected))
	if err != nil {
		return false, 0, fmt.Errorf("resolve stale drift: %w", err)
	}
	resolved = int(tag.RowsAffected())

	if r.Drifted {
		var peer *int64
		if r.Source == drift.FromPeer {
			// Recorded so the row can say what it was compared against, even if
			// the configuration changes later.
			var p int64
			if err := tx.QueryRow(ctx,
				`SELECT expected_peer_id FROM schemaver.database WHERE id = $1`,
				databaseID).Scan(&p); err == nil {
				peer = &p
			}
		}
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO schemaver.drift
			    (database_id, observed_fingerprint, expected_fingerprint,
			     expected_source, peer_database_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (database_id, observed_fingerprint, expected_fingerprint)
			    WHERE status = 'open'
			DO UPDATE SET last_seen = now()
			RETURNING (xmax = 0)`,
			databaseID, string(r.Observed), string(r.Expected),
			string(r.Source), peer).Scan(&inserted)
		if err != nil {
			return false, 0, fmt.Errorf("record drift: %w", err)
		}
		opened = inserted
	}

	if err := tx.Commit(ctx); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}

	// Recorded here rather than in the worker: this is where the change is
	// known, and the worker would otherwise have to look up a project purely to
	// describe something the store already has in hand. Drift is also the only
	// activity nobody asked for, which makes it the most worth surfacing —
	// everything else in the log is somebody's deliberate action.
	if opened {
		s.recordFor(ctx, databaseID, Warn("drift.detected", fmt.Sprintf(
			"observed %s, expected %s (%s)",
			r.Observed.Short(), r.Expected.Short(), r.Source)).
			OnDatabase(databaseID).
			With(map[string]any{
				"observed": string(r.Observed), "expected": string(r.Expected),
				"source": string(r.Source), "reason": r.Reason,
			}))
	}
	if resolved > 0 {
		s.recordFor(ctx, databaseID, Info("drift.resolved", fmt.Sprintf(
			"back in line at %s; %d drift record(s) closed",
			r.Observed.Short(), resolved)).
			OnDatabase(databaseID))
	}
	return opened, resolved, nil
}

// CurrentFingerprint returns the last schema observed for a database.
func (s *Store) CurrentFingerprint(ctx context.Context, databaseID int64) (schema.Version, error) {
	var fingerprint *string
	if err := s.pool.QueryRow(ctx,
		`SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&fingerprint); err != nil {
		return "", fmt.Errorf("read current fingerprint: %w", err)
	}
	if fingerprint == nil {
		return "", nil
	}
	return schema.Version(*fingerprint), nil
}
