package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ChainStep is one database a change would pass through.
type ChainStep struct {
	DatabaseID  int64
	Name        string
	Instance    string
	Environment string
	Position    int
	// Observed is the schema last read there. A step whose schema differs from
	// the one below it is already out of line, and the change will not reach it
	// until that is sorted out.
	Observed schema.Version
}

// Chain returns the databases a change written for one database would pass
// through, in order, starting with that database itself.
//
// Walked along the Follows mapping in the direction a change travels, which is
// the opposite of the way the mapping points: production follows staging, so
// from staging the next step is whatever follows it. D-024 already gates each
// on the one below, so this only has to name them.
//
// Bounded at eight. The mapping refuses cycles, so this terminates on its own;
// the limit is there because a chain that long is a configuration mistake
// rather than a pipeline, and walking it forever would be a poor way to find
// that out.
func (s *Scope) Chain(ctx context.Context, databaseID int64) ([]ChainStep, error) {
	rows, err := s.store.pool.Query(ctx, `
		WITH RECURSIVE chain(id, position) AS (
			SELECT $1::bigint, 0
			UNION ALL
			SELECT d.id, c.position + 1
			  FROM schemaver.database d
			  JOIN chain c ON d.expected_peer_id = c.id
			 WHERE c.position < 8
		)
		SELECT c.position, d.id, d.name, i.name, COALESCE(e.name, ''),
		       COALESCE(d.current_fingerprint, '')
		  FROM chain c
		  JOIN schemaver.database d ON d.id = c.id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		 WHERE i.project_id = ANY($2)
		   AND d.managed AND d.retired_at IS NULL AND d.archived_at IS NULL
		 ORDER BY c.position`, databaseID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("walk the promotion chain: %w", err)
	}
	defer rows.Close()

	var out []ChainStep
	for rows.Next() {
		var st ChainStep
		var fingerprint string
		if err := rows.Scan(&st.Position, &st.DatabaseID, &st.Name, &st.Instance,
			&st.Environment, &fingerprint); err != nil {
			return nil, fmt.Errorf("scan chain step: %w", err)
		}
		st.Observed = schema.Version(fingerprint)
		out = append(out, st)
	}
	return out, rows.Err()
}

// RequestTarget is one database a request will reach, and how far it has got.
//
// Named apart from the worker's Target, which is a database about to be
// observed. They are different ideas that happen to share a word.
type RequestTarget struct {
	DatabaseID  int64
	Name        string
	Instance    string
	Environment string
	Position    int
	// Reached reports that this database is already at the migration's target
	// schema, whether this request put it there or it arrived another way.
	Reached bool
	// Current is where it actually sits.
	Current schema.Version
}

// Targets lists the databases a request will reach, in order.
func (s *Scope) Targets(ctx context.Context, requestID int64) ([]RequestTarget, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT t.position, d.id, d.name, i.name, COALESCE(e.name, ''),
		       COALESCE(d.current_fingerprint, ''),
		       COALESCE(d.current_fingerprint, '') = COALESCE(m.to_fingerprint, '~')
		  FROM schemaver.change_request_target t
		  JOIN schemaver.database d ON d.id = t.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		  JOIN schemaver.change_request r ON r.id = t.change_request_id
		  LEFT JOIN schemaver.migration m
		         ON m.change_request_id = r.id AND m.superseded_at IS NULL
		 WHERE t.change_request_id = $1 AND r.project_id = ANY($2)
		 ORDER BY t.position`, requestID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("load targets: %w", err)
	}
	defer rows.Close()

	var out []RequestTarget
	for rows.Next() {
		var t RequestTarget
		var fingerprint string
		if err := rows.Scan(&t.Position, &t.DatabaseID, &t.Name, &t.Instance,
			&t.Environment, &fingerprint, &t.Reached); err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}
		t.Current = schema.Version(fingerprint)
		out = append(out, t)
	}
	return out, rows.Err()
}

// setTargets records the databases a request will reach.
//
// Always includes the database the request was written for, at position zero,
// whatever was ticked: a change has to land somewhere, and the migration is
// generated against that database's schema.
func (s *Scope) setTargets(ctx context.Context, tx pgx.Tx, requestID, first int64, also []int64) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM schemaver.change_request_target WHERE change_request_id = $1`,
		requestID); err != nil {
		return fmt.Errorf("clear targets: %w", err)
	}

	chain, err := s.Chain(ctx, first)
	if err != nil {
		return err
	}
	wanted := map[int64]bool{first: true}
	for _, id := range also {
		wanted[id] = true
	}

	// Positioned by the chain rather than by the order they arrived, so the
	// sequence is the promotion order and not whatever the form happened to
	// submit. A ticked database that is not on the chain is ignored: it has no
	// place in the order, and guessing one would invent a promotion nobody
	// configured.
	position := 0
	for _, step := range chain {
		if !wanted[step.DatabaseID] {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO schemaver.change_request_target
			       (change_request_id, database_id, position)
			VALUES ($1, $2, $3)`, requestID, step.DatabaseID, position); err != nil {
			return fmt.Errorf("record target %s: %w", step.Name, err)
		}
		position++
	}
	return nil
}

// ErrPipelineDone is returned when every target has reached the migration's
// schema, so there is nothing left to run.
var ErrPipelineDone = fmt.Errorf("every database in this pipeline is already at the target schema")

// nextTarget is the database a press of the execute button means.
//
// The first target, in promotion order, that has not reached the migration's
// schema. A target already there is skipped rather than run again — it may have
// got there by this pipeline a moment ago, or by somebody else's change, and
// either way applying the statements a second time would fail on the first
// column that already exists.
func (s *Scope) nextTarget(ctx context.Context, requestID, migrationID int64) (int64, error) {
	var id int64
	err := s.store.pool.QueryRow(ctx, `
		SELECT t.database_id
		  FROM schemaver.change_request_target t
		  JOIN schemaver.database d ON d.id = t.database_id
		  JOIN schemaver.migration m ON m.id = $2
		 WHERE t.change_request_id = $1
		   AND COALESCE(d.current_fingerprint, '') <> m.to_fingerprint
		 ORDER BY t.position
		 LIMIT 1`, requestID, migrationID).Scan(&id)
	if err != nil {
		return 0, ErrPipelineDone
	}
	return id, nil
}

// PipelineComplete reports that every target has reached the migration's
// schema.
func (s *Scope) PipelineComplete(ctx context.Context, requestID, migrationID int64) (bool, error) {
	var remaining int
	if err := s.store.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM schemaver.change_request_target t
		  JOIN schemaver.database d ON d.id = t.database_id
		  JOIN schemaver.migration m ON m.id = $2
		 WHERE t.change_request_id = $1
		   AND COALESCE(d.current_fingerprint, '') <> m.to_fingerprint`,
		requestID, migrationID).Scan(&remaining); err != nil {
		return false, fmt.Errorf("count remaining targets: %w", err)
	}
	return remaining == 0, nil
}

// PipelineProgress reports how many targets have reached the migration's schema
// and how many have not.
//
// justRan names a database the caller has itself seen arrive there. The
// recorded fingerprint is updated by a read queued after the run, which has
// almost certainly not happened yet — so counting from the record alone would
// report the database whose migration just succeeded as still waiting, and say
// "two to go" the moment one of two finished. The executor verified that
// fingerprint before returning; this trusts it. Pass zero when there is
// nothing to vouch for.
func (s *Store) PipelineProgress(ctx context.Context, requestID, migrationID, justRan int64) (done, left int, err error) {
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE reached),
		       count(*) FILTER (WHERE NOT reached)
		  FROM (
			SELECT d.id = $3
			    OR COALESCE(d.current_fingerprint, '') = m.to_fingerprint AS reached
			  FROM schemaver.change_request_target t
			  JOIN schemaver.database d ON d.id = t.database_id
			  JOIN schemaver.migration m ON m.id = $2
			 WHERE t.change_request_id = $1
		  ) x`, requestID, migrationID, justRan).
		Scan(&done, &left); err != nil {
		return 0, 0, fmt.Errorf("measure pipeline progress: %w", err)
	}
	return done, left, nil
}
