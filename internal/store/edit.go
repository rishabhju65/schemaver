package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/auth"
)

// ErrNotEditable is returned when the statements can no longer be changed.
var ErrNotEditable = errors.New(
	"this migration's statements can no longer be edited; it is queued, running " +
		"or finished")

// ErrNoSuchStatement is returned when an edit names a statement that is not there.
var ErrNoSuchStatement = errors.New("no such statement in this migration")

// EditStatement replaces the SQL of one statement, forward or revert.
//
// Only an administrator, and only while the migration is still under review.
// The generator gets things wrong — a cast with no USING clause, a change it
// can only render as a comment — and D-001 has always said a human must be able
// to correct the generated plan. This is that, and until now there was no way
// to do it at all.
//
// Every edit recomputes the plan digest, and that is the whole safety story.
// Approvals are recorded against the digest, so editing withdraws them without
// anything having to go looking for them; the proof is reset for the same
// reason, because a migration whose statements have changed has not been proven
// whatever was established about the statements it used to hold.
func (s *Scope) EditStatement(ctx context.Context, actorID, migrationID int64, revert bool, ordinal int, sql string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(sql) == "" {
		return errors.New("a statement cannot be empty; to remove a change, regenerate the migration")
	}

	var projectID, requestID int64
	var state string
	err := s.store.pool.QueryRow(ctx, `
		SELECT r.project_id, r.id, r.state
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		 WHERE m.id = $1 AND r.project_id = ANY($2) AND m.superseded_at IS NULL`,
		migrationID, s.projects).Scan(&projectID, &requestID, &state)
	if err != nil {
		return fmt.Errorf("load migration for editing: %w", err)
	}

	// Once it is queued the statements are on their way to a database, and
	// after that they are what ran. Editing either would make the record of
	// what happened disagree with what happened.
	switch state {
	case "READY_TO_EXECUTE", "EXECUTING", "COMPLETED", "CLOSED", "NEEDS_ATTENTION":
		return ErrNotEditable
	}

	role, member, err := s.store.MemberOf(ctx, actorID, projectID)
	if err != nil {
		return err
	}
	if !member || role != auth.Admin {
		return ErrNotAdmin
	}

	table := "schemaver.migration_step"
	if revert {
		table = "schemaver.migration_revert_step"
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Transactionality is re-derived rather than preserved. An administrator
	// who edits a plain ALTER into a concurrent index build has changed whether
	// the statement can run inside a transaction, and carrying the old answer
	// forward would have the executor put it in one and fail.
	tag, err := tx.Exec(ctx, `
		UPDATE `+table+`
		   SET sql = $3, transactional = $4
		 WHERE migration_id = $1 AND ordinal = $2`,
		migrationID, ordinal, sql, !concurrent(sql))
	if err != nil {
		return fmt.Errorf("edit statement: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchStatement
	}

	// Only the forward statements have a proven fingerprint chain, and it
	// described the statements as they were.
	if !revert {
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.migration_step SET expected_after = NULL
			 WHERE migration_id = $1`, migrationID); err != nil {
			return fmt.Errorf("clear the fingerprint chain: %w", err)
		}
	}

	digest, err := recomputeDigest(ctx, tx, migrationID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.migration
		   SET plan_digest = $2, proof_state = 'pending', proof_reason = NULL,
		       proved_at = NULL
		 WHERE id = $1`, migrationID, digest); err != nil {
		return fmt.Errorf("reset the proof: %w", err)
	}

	// Back to waiting on its proof, which is where a migration whose statements
	// nobody has checked belongs.
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'STAGE_SANITY',
		       state_reason = 'statements were edited; proving them again',
		       updated_at = now()
		 WHERE id = $1`, requestID); err != nil {
		return fmt.Errorf("advance request: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, weight, idempotency_key)
		VALUES ('prove', 'migration', $1, 1, $2)
		ON CONFLICT (idempotency_key) DO UPDATE
		   SET state = 'pending', run_after = now(), error = NULL,
		       finished_at = NULL`,
		migrationID, fmt.Sprintf("prove:%d", migrationID)); err != nil {
		return fmt.Errorf("re-queue the proof: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	which := "statement"
	if revert {
		which = "revert statement"
	}
	s.record(ctx, Warn("statement.edited", fmt.Sprintf(
		"%s %d edited by hand; approvals withdrawn and the migration must be "+
			"proven again", which, ordinal)).
		By(actorID).
		OnRequest(requestID).
		OnMigration(migrationID).
		At(ordinal))
	return nil
}

// recomputeDigest rebuilds the plan digest from the statements as they now are.
//
// Read back inside the same transaction as the edit, rather than computed from
// what the caller sent. The digest has to describe what is stored, and the only
// way to be certain of that is to read what is stored.
func recomputeDigest(ctx context.Context, tx pgx.Tx, migrationID int64) (string, error) {
	forward, err := digestSteps(ctx, tx, "schemaver.migration_step", migrationID)
	if err != nil {
		return "", err
	}
	revert, err := digestSteps(ctx, tx, "schemaver.migration_revert_step", migrationID)
	if err != nil {
		return "", err
	}
	revertSteps := make([]RevertStep, len(revert))
	for i, st := range revert {
		revertSteps[i] = RevertStep{Ordinal: st.Ordinal, SQL: st.SQL}
	}
	return planDigest(forward, revertSteps), nil
}

func digestSteps(ctx context.Context, tx pgx.Tx, table string, migrationID int64) ([]Step, error) {
	rows, err := tx.Query(ctx,
		`SELECT ordinal, sql FROM `+table+`
		  WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("read %s for digest: %w", table, err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Ordinal, &st.SQL); err != nil {
			return nil, fmt.Errorf("scan step for digest: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// concurrent reports a statement that refuses to run inside a transaction.
func concurrent(sql string) bool {
	return strings.Contains(strings.ToUpper(sql), "CONCURRENTLY")
}

// WriteRevert replaces the way back with what somebody has written.
//
// Taken as one block and split on statement boundaries rather than edited a
// statement at a time. Somebody writing a rollback is writing a script: they
// think in whole sequences, paste from an editor, and reorder freely, and
// making them do that a box at a time would be the interface fighting the task.
//
// Anyone who can change the request may write it — this is authoring, not
// approving. Like every edit it recomputes the plan digest, so writing or
// rewriting a revert withdraws the approvals that were given for the previous
// one and sends the migration back to be proven.
func (s *Scope) WriteRevert(ctx context.Context, actorID, migrationID int64, sql string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}

	var projectID, requestID int64
	var state string
	err := s.store.pool.QueryRow(ctx, `
		SELECT r.project_id, r.id, r.state
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		 WHERE m.id = $1 AND r.project_id = ANY($2) AND m.superseded_at IS NULL`,
		migrationID, s.projects).Scan(&projectID, &requestID, &state)
	if err != nil {
		return fmt.Errorf("load migration: %w", err)
	}
	switch state {
	case "READY_TO_EXECUTE", "EXECUTING", "COMPLETED", "CLOSED", "NEEDS_ATTENTION":
		return ErrNotEditable
	}

	statements := SplitStatements(sql)

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Replaced wholesale rather than merged. A revert is one script, and
	// reconciling a rewrite against the statements it replaced would be
	// guessing at an intent the author has already expressed plainly.
	if _, err := tx.Exec(ctx,
		`DELETE FROM schemaver.migration_revert_step WHERE migration_id = $1`,
		migrationID); err != nil {
		return fmt.Errorf("clear the previous revert: %w", err)
	}
	for i, st := range statements {
		if _, err := tx.Exec(ctx, `
			INSERT INTO schemaver.migration_revert_step
			    (migration_id, ordinal, sql, change_id, transactional)
			VALUES ($1, $2, $3, 'revert', $4)`,
			migrationID, i+1, st, !concurrent(st)); err != nil {
			return fmt.Errorf("store revert statement %d: %w", i+1, err)
		}
	}

	digest, err := recomputeDigest(ctx, tx, migrationID)
	if err != nil {
		return err
	}
	authored := len(statements) > 0
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.migration
		   SET plan_digest = $2,
		       proof_state = 'pending', proof_reason = NULL, proved_at = NULL,
		       revert_proof_state = 'pending', revert_proof_reason = NULL,
		       revert_authored_at = CASE WHEN $3 THEN now() END,
		       revert_author_id = CASE WHEN $3 THEN $4::bigint END
		 WHERE id = $1`, migrationID, digest, authored, actorID); err != nil {
		return fmt.Errorf("record the revert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	what := fmt.Sprintf("wrote a revert of %d statement(s)", len(statements))
	if !authored {
		what = "removed the revert"
	}
	s.record(ctx, Warn("revert.written", what+
		"; approvals withdrawn and the migration must be proven again").
		By(actorID).
		OnRequest(requestID).
		OnMigration(migrationID))
	return nil
}
