package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// rebasable are the states where rebuilding a request against a moved database
// is the right thing to do on its own.
//
// EXECUTING is excluded because the executor holds the migration and checks the
// starting fingerprint itself under the lock; rewriting the plan underneath it
// would be a race against the one place that is already careful about this.
//
// FAILED and NEEDS_ATTENTION are excluded because they already have somebody's
// attention. Rewriting a plan while a person is reading it to work out what
// went wrong replaces the evidence with something else.
//
// STALE is included, which is the point: a request stranded by an earlier move
// recovers by itself the next time the database settles.
var rebasable = []string{
	"INITIATED", "STAGE_SANITY", "IN_REVIEW", "CHANGES_REQUESTED",
	"READY_TO_EXECUTE", "STALE",
}

// EnqueueRebase asks for a database's open requests to be rebuilt.
//
// Keyed by database so that a burst of changes collapses into one pass: the
// work is "bring everything up to date with where this database is now", and
// doing it once after three changes is the same as doing it three times.
func (s *Store) EnqueueRebase(ctx context.Context, databaseID int64) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, weight, idempotency_key)
		VALUES ('rebase', 'database', $1, 1, $2)
		ON CONFLICT (idempotency_key) DO UPDATE
		   SET state = 'pending', run_after = now(), error = NULL,
		       finished_at = NULL`,
		databaseID, fmt.Sprintf("rebase:%d", databaseID)); err != nil {
		return fmt.Errorf("queue the rebase: %w", err)
	}
	return nil
}

// Rebase is what one pass did.
type Rebase struct {
	Rebuilt  int
	Conflict int
}

// RebaseOpenRequests rebuilds every open request against where its target
// database now is.
//
// A migration is a plan from one exact schema to another. When the database
// moves — another request ran, or somebody changed it by hand — every other
// open request against it is planning from a schema that is no longer there.
// Until now that was noticed only when somebody tried to run one, and the
// executor refused it under the lock: correct, and the worst possible moment to
// find out.
//
// Rebuilding withdraws approvals, and that is intended rather than tolerated.
// Approvals are counted only against the plan digest they were given for, so a
// rebuilt plan leaves them behind with nothing having to revoke them, and the
// request drops back to review. Somebody approved those statements; these are
// different statements.
//
// Where the rebuild cannot be done — the branch now genuinely conflicts with
// what the database did — the request is marked with the conflict rather than
// left looking ready. That is the case this exists to surface.
func (s *Store) RebaseOpenRequests(ctx context.Context, databaseID int64) (Rebase, error) {
	var out Rebase

	var projectID int64
	err := s.pool.QueryRow(ctx, `
		SELECT i.project_id FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1`, databaseID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The database went between the change being noticed and this running —
		// removed, or its server archived. Nothing to rebuild and nothing
		// wrong: reported as a clean pass rather than an error, because a
		// failing job is retried, and no number of retries brings a database
		// back.
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("find the project for database %d: %w", databaseID, err)
	}

	// Only those whose plan actually starts somewhere the database no longer
	// is. A request generated after the move is already current, and rebuilding
	// it would withdraw its approvals for nothing.
	rows, err := s.pool.Query(ctx, `
		SELECT r.id
		  FROM schemaver.change_request r
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.migration m ON m.change_request_id = r.id
		                            AND m.superseded_at IS NULL
		 WHERE r.database_id = $1
		   AND r.state = ANY($2)
		   AND d.current_fingerprint IS NOT NULL
		   AND m.from_fingerprint IS DISTINCT FROM d.current_fingerprint
		 ORDER BY r.id`, databaseID, rebasable)
	if err != nil {
		return out, fmt.Errorf("list requests to rebase: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, fmt.Errorf("scan request id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	scope := s.ForProject(projectID)
	for _, id := range ids {
		// Zero actor: schemaver did this, not a person.
		if err := scope.regenerate(ctx, 0, id); err != nil {
			out.Conflict++
			if merr := s.markUnrebasable(ctx, databaseID, id, err); merr != nil {
				return out, merr
			}
			continue
		}
		out.Rebuilt++
	}
	return out, nil
}

// markUnrebasable records why a request could not be brought up to date.
//
// STALE rather than an error state, because nothing went wrong: the request is
// simply planning from somewhere the database has left, and what it needs is a
// person to decide what it should do instead.
func (s *Store) markUnrebasable(ctx context.Context, databaseID, requestID int64, cause error) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'STALE', state_reason = $2, updated_at = now()
		 WHERE id = $1`, requestID, cause.Error()); err != nil {
		return fmt.Errorf("mark request %d stale: %w", requestID, err)
	}
	s.recordFor(ctx, databaseID, Warn("request.rebase_failed", cause.Error()).
		OnRequest(requestID).OnDatabase(databaseID))
	return nil
}
