package store

import (
	"context"
	"fmt"
)

// Retirement is what retiring a database costs: either what it would close, or
// what it did.
type Retirement struct {
	DatabaseName   string
	CancelledJobs  int
	ClosedRequests int
	ClosedDrift    int
	UnpairedPeers  int
}

// Quiet reports that retiring would close nothing, so the confirmation can say
// so plainly instead of listing four zeroes.
func (r Retirement) Quiet() bool {
	return r.CancelledJobs == 0 && r.ClosedRequests == 0 &&
		r.ClosedDrift == 0 && r.UnpairedPeers == 0
}

// PreviewRetirement counts what retiring would close, changing nothing.
//
// The same four counts the act itself reports, from the same predicates, so the
// confirmation cannot promise one thing and the action do another. Everything
// else in this product shows the consequences before asking — the review page
// exists for that reason — and standing a database down deserves the same.
func (s *Scope) PreviewRetirement(ctx context.Context, databaseID int64) (*Retirement, error) {
	if err := s.requireWritable(ctx, databaseID); err != nil {
		return nil, err
	}
	out := &Retirement{}
	err := s.store.pool.QueryRow(ctx, `
		SELECT d.name,
		       (SELECT count(*) FROM schemaver.job j
		         WHERE j.state IN ('pending', 'running')
		           AND ((j.target_kind = 'database' AND j.target_id = d.id)
		             OR (j.target_kind = 'migration' AND EXISTS (
		                   SELECT 1 FROM schemaver.migration m
		                     JOIN schemaver.change_request r ON r.id = m.change_request_id
		                    WHERE m.id = j.target_id AND r.database_id = d.id)))),
		       (SELECT count(*) FROM schemaver.change_request r
		         WHERE r.database_id = d.id
		           AND r.state NOT IN ('COMPLETED', 'CLOSED')),
		       (SELECT count(*) FROM schemaver.drift f
		         WHERE f.database_id = d.id AND f.status = 'open'),
		       (SELECT count(*) FROM schemaver.database p
		         WHERE p.expected_peer_id = d.id)
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1 AND i.project_id = ANY($2)`, databaseID, s.projects).
		Scan(&out.DatabaseName, &out.CancelledJobs, &out.ClosedRequests,
			&out.ClosedDrift, &out.UnpairedPeers)
	if err != nil {
		return nil, fmt.Errorf("preview retirement: %w", err)
	}
	return out, nil
}

// RetireDatabase stands a database down: schemaver stops watching it, stops
// accepting changes against it, and keeps everything it already knows.
//
// The cascade is the point. Leaving queued jobs, open change requests and open
// drift behind would mean a retired database still produced work, alerts and
// buttons that could never succeed — the request page would offer an execute
// button whose job would be claimed and then fail its precondition forever.
// Each of those is closed with a reason naming this, so nothing simply
// disappears without explanation.
func (s *Scope) RetireDatabase(ctx context.Context, actorID, databaseID int64, reason string) (*Retirement, error) {
	if err := s.requireWrite(); err != nil {
		return nil, err
	}
	// Writable rather than merely visible: retiring something already retired
	// should say so rather than silently re-stamping it.
	if err := s.requireWritable(ctx, databaseID); err != nil {
		return nil, err
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out := &Retirement{}
	if err := tx.QueryRow(ctx, `
		UPDATE schemaver.database
		   SET retired_at = now(), retired_by = $2, retired_reason = NULLIF($3, ''),
		       managed = false, expected_peer_id = NULL
		 WHERE id = $1
		RETURNING name`, databaseID, actorID, reason).Scan(&out.DatabaseName); err != nil {
		return nil, fmt.Errorf("retire database: %w", err)
	}

	// Databases comparing themselves against this one lose their expectation.
	// Left in place, they would drift against a fingerprint that can never move
	// again and report it forever.
	tag, err := tx.Exec(ctx, `
		UPDATE schemaver.database SET expected_peer_id = NULL
		 WHERE expected_peer_id = $1`, databaseID)
	if err != nil {
		return nil, fmt.Errorf("unpair peers: %w", err)
	}
	out.UnpairedPeers = int(tag.RowsAffected())

	// Queued and leased work, including a migration already waiting to run.
	tag, err = tx.Exec(ctx, `
		UPDATE schemaver.job j
		   SET state = 'cancelled', finished_at = now(),
		       error = 'the database was retired'
		 WHERE j.state IN ('pending', 'running')
		   AND ((j.target_kind = 'database' AND j.target_id = $1)
		     OR (j.target_kind = 'migration' AND EXISTS (
		           SELECT 1 FROM schemaver.migration m
		             JOIN schemaver.change_request r ON r.id = m.change_request_id
		            WHERE m.id = j.target_id AND r.database_id = $1)))`, databaseID)
	if err != nil {
		return nil, fmt.Errorf("cancel jobs: %w", err)
	}
	out.CancelledJobs = int(tag.RowsAffected())

	tag, err = tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'CLOSED', state_reason = 'the target database was retired',
		       updated_at = now()
		 WHERE database_id = $1
		   AND state NOT IN ('COMPLETED', 'CLOSED')`, databaseID)
	if err != nil {
		return nil, fmt.Errorf("close requests: %w", err)
	}
	out.ClosedRequests = int(tag.RowsAffected())

	tag, err = tx.Exec(ctx, `
		UPDATE schemaver.drift
		   SET status = 'resolved', resolved_at = now()
		 WHERE database_id = $1 AND status = 'open'`, databaseID)
	if err != nil {
		return nil, fmt.Errorf("close drift: %w", err)
	}
	out.ClosedDrift = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, Warn("database.retired", fmt.Sprintf(
		"%s retired: %d job(s) cancelled, %d request(s) closed, %d drift record(s) closed",
		out.DatabaseName, out.CancelledJobs, out.ClosedRequests, out.ClosedDrift)).
		By(actorID).
		OnDatabase(databaseID).
		With(map[string]any{
			"reason": reason, "cancelled_jobs": out.CancelledJobs,
			"closed_requests": out.ClosedRequests, "closed_drift": out.ClosedDrift,
			"unpaired_peers": out.UnpairedPeers,
		}))
	return out, nil
}

// RestoreDatabase brings a retired database back under management.
//
// The cached fingerprint is discarded rather than trusted. It describes the
// database as it was when schemaver stopped watching, and the whole reason a
// migration names the state it starts from is that acting on a stale one is how
// data gets destroyed. Clearing it forces a full read before anything can be
// proposed.
//
// What the cascade closed is not reopened. A change request closed because its
// target was retired was closed against a schema nobody has looked at since;
// reviving it would revive an approval made in a world that has moved on.
func (s *Scope) RestoreDatabase(ctx context.Context, actorID, databaseID int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}

	var name string
	err := s.store.pool.QueryRow(ctx, `
		UPDATE schemaver.database d
		   SET retired_at = NULL, retired_by = NULL, retired_reason = NULL,
		       managed = true,
		       current_fingerprint = NULL, current_snapshot_id = NULL,
		       probe_digest = NULL, last_read_at = NULL
		  FROM schemaver.instance i
		 WHERE d.id = $1 AND d.instance_id = i.id AND i.project_id = ANY($2)
		   AND d.retired_at IS NOT NULL AND d.archived_at IS NULL
		RETURNING d.name`, databaseID, s.projects).Scan(&name)
	if err != nil {
		return fmt.Errorf("restore database: %w", err)
	}

	s.record(ctx, Info("database.restored",
		name+" is managed again; its schema will be read afresh before anything "+
			"can be proposed against it").
		By(actorID).
		OnDatabase(databaseID))
	return nil
}
