package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ProofTask is everything needed to prove one migration.
type ProofTask struct {
	MigrationID int64
	RequestID   int64
	ProjectID   int64
	DatabaseID  int64
	From, To    schema.Version
	// BaseDDL rebuilds the starting schema in a throwaway database. Rendered
	// from the stored blob rather than dumped from the real database: the blob
	// is what the migration was generated against, and dumping again could
	// quietly prove a different starting point than the one declared.
	BaseDDL    string
	Statements []string
	// Revert is the way back, proven as a round trip: applied to the shadow
	// once the forward statements have, which is the only state it is for.
	Revert []string
}

// ErrNoProofNeeded is returned when the migration has been superseded, or its
// request closed, since the proof was queued.
var ErrNoProofNeeded = errors.New("this migration no longer needs proving")

// LoadProofTask assembles a proof from a migration id.
func (s *Store) LoadProofTask(ctx context.Context, migrationID int64) (*ProofTask, error) {
	t := &ProofTask{MigrationID: migrationID}
	var from, to string
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, i.project_id, d.id, m.from_fingerprint, m.to_fingerprint
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE m.id = $1 AND m.superseded_at IS NULL
		   AND r.state NOT IN ('CLOSED', 'COMPLETED')`, migrationID).
		Scan(&t.RequestID, &t.ProjectID, &t.DatabaseID, &from, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoProofNeeded
	}
	if err != nil {
		return nil, fmt.Errorf("load proof task: %w", err)
	}
	t.From, t.To = schema.Version(from), schema.Version(to)

	rows, err := s.pool.Query(ctx, `
		SELECT sql FROM schemaver.migration_step
		 WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("load statements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			return nil, fmt.Errorf("scan statement: %w", err)
		}
		t.Statements = append(t.Statements, sql)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	revert, err := s.pool.Query(ctx, `
		SELECT sql FROM schemaver.migration_revert_step
		 WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("load revert statements: %w", err)
	}
	defer revert.Close()
	for revert.Next() {
		var sql string
		if err := revert.Scan(&sql); err != nil {
			return nil, fmt.Errorf("scan revert statement: %w", err)
		}
		t.Revert = append(t.Revert, sql)
	}
	if err := revert.Err(); err != nil {
		return nil, err
	}

	// Rendered here rather than by the caller, so the unscoped blob read stays
	// inside the store. A worker serves every project at once and holds no
	// scope; resolving the migration id above is what establishes which project
	// this belongs to.
	var canonical []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT canonical FROM schemaver.schema_blob WHERE fingerprint = $1`,
		from).Scan(&canonical); err != nil {
		return nil, fmt.Errorf("load the starting schema %s: %w", t.From.Short(), err)
	}
	var base schema.Schema
	if err := json.Unmarshal(canonical, &base); err != nil {
		return nil, fmt.Errorf("decode the starting schema %s: %w", t.From.Short(), err)
	}
	t.BaseDDL = render.Schema(&base)
	return t, nil
}

// RecordProof stores the outcome and, when it passed, the fingerprint each
// statement is expected to leave behind.
//
// The chain is written in the same transaction as the verdict. A migration
// marked proven whose chain was half-written would be worse than one never
// proven at all: the executor would index into it and place a failure at the
// wrong statement.
func (s *Store) RecordProof(ctx context.Context, migrationID int64, state, reason string, after []schema.Version) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.migration
		   SET proof_state = $2, proof_reason = NULLIF($3, ''), proved_at = now()
		 WHERE id = $1`, migrationID, state, reason); err != nil {
		return fmt.Errorf("record proof: %w", err)
	}
	for i, fp := range after {
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.migration_step SET expected_after = $3
			 WHERE migration_id = $1 AND ordinal = $2`,
			migrationID, i+1, string(fp)); err != nil {
			return fmt.Errorf("record expected fingerprint for statement %d: %w", i+1, err)
		}
	}

	// A failed rehearsal goes back to its author: a migration that does not
	// produce the schema it declares is not something to put in front of
	// production.
	if state == "failed" {
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.change_request r
			   SET state = 'CHANGES_REQUESTED', state_reason = NULLIF($2, ''),
			       updated_at = now()
			  FROM schemaver.migration m
			 WHERE m.id = $1 AND r.id = m.change_request_id
			   AND r.state = 'STAGE_SANITY'`, migrationID, reason); err != nil {
			return fmt.Errorf("advance request: %w", err)
		}
		return tx.Commit(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the proof: %w", err)
	}

	// A passing rehearsal used to move the request straight to
	// READY_TO_EXECUTE, on the reasoning that it had been approved to get here
	// and so passing meant ready to run. Approval is one of the gate's
	// conditions and not the only one: the way back, a blocking review, an
	// unanswered rename question and the environment below are all still
	// outstanding at this point, and none of them is consulted here.
	//
	// So a request could read READY_TO_EXECUTE — "approved and queued" — while
	// the gate beneath it on the same page said it could not run and named
	// something nobody had done yet. Two independent answers to one question,
	// and the louder of the two was the wrong one.
	//
	// The gate is asked instead. Where it agrees the state says so; where it
	// does not, the request goes back to review carrying the gate's own reason,
	// which is the thing a reader can act on.
	return s.settleAfterProof(ctx, migrationID)
}

// settleAfterProof puts a rehearsed request where the gate says it belongs.
//
// Separate from the proof's own transaction because the gate reads the proof:
// evaluated inside it, it would be deciding against the state of the world
// before the rehearsal was recorded.
func (s *Store) settleAfterProof(ctx context.Context, migrationID int64) error {
	var requestID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT change_request_id FROM schemaver.migration WHERE id = $1`,
		migrationID).Scan(&requestID); err != nil {
		return fmt.Errorf("find the request for migration %d: %w", migrationID, err)
	}

	gate, err := s.ApprovalStateFor(ctx, requestID)
	if err != nil {
		return fmt.Errorf("evaluate the gate: %w", err)
	}

	next, why := "IN_REVIEW", gate.Reason
	if gate.Executable {
		next, why = "READY_TO_EXECUTE", ""
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = $2, state_reason = NULLIF($3, ''), updated_at = now()
		 WHERE id = $1 AND state = 'STAGE_SANITY'`, requestID, next, why); err != nil {
		return fmt.Errorf("advance request: %w", err)
	}
	return nil
}

// ExpectedAfter returns the fingerprint chain for a migration, or nil if it has
// not been proven.
func (s *Store) ExpectedAfter(ctx context.Context, migrationID int64) ([]schema.Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(expected_after, '') FROM schemaver.migration_step
		 WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("load expected fingerprints: %w", err)
	}
	defer rows.Close()
	var out []schema.Version
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scan expected fingerprint: %w", err)
		}
		out = append(out, schema.Version(fp))
	}
	return out, rows.Err()
}

// RecordRevertProof stores what the round trip established about the way back.
func (s *Store) RecordRevertProof(ctx context.Context, migrationID int64, state, reason string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.migration
		   SET revert_proof_state = $2, revert_proof_reason = NULLIF($3, '')
		 WHERE id = $1`, migrationID, state, reason); err != nil {
		return fmt.Errorf("record revert proof: %w", err)
	}
	return nil
}
