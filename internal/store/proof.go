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

	// A request waiting on its proof moves on. Passing opens it for review;
	// failing sends it back to its author, because a migration that does not
	// produce the declared schema is not something to ask people to read.
	next, why := "IN_REVIEW", ""
	switch state {
	case "failed":
		next, why = "CHANGES_REQUESTED", reason
	case "unproven":
		why = reason
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request r
		   SET state = $2, state_reason = NULLIF($3, ''), updated_at = now()
		  FROM schemaver.migration m
		 WHERE m.id = $1 AND r.id = m.change_request_id
		   AND r.state = 'STAGE_SANITY'`, migrationID, next, why); err != nil {
		return fmt.Errorf("advance request: %w", err)
	}
	return tx.Commit(ctx)
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
