package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ErrNotExecutable is returned when the gate is shut.
var ErrNotExecutable = errors.New("this migration is not executable")

// EnqueueExecution queues a migration for execution, if the gate allows it.
//
// The gate is re-evaluated here rather than trusted from whenever the page was
// rendered: an approval can be withdrawn, a reviewer can object, and the
// migration can be regenerated between a person seeing a button and pressing it.
func (s *Scope) EnqueueExecution(ctx context.Context, requestID int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	state, err := s.ApprovalState(ctx, requestID)
	if err != nil {
		return err
	}
	if !state.Executable {
		return fmt.Errorf("%w: %s", ErrNotExecutable, state.Reason)
	}

	var instanceID int64
	var weight int
	if err := s.store.pool.QueryRow(ctx, `
		SELECT i.id, m.weight
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE m.id = $1`, state.MigrationID).Scan(&instanceID, &weight); err != nil {
		return fmt.Errorf("load migration target: %w", err)
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Keyed on the migration, so pressing the button twice enqueues once. The
	// key is built here rather than concatenated in SQL: reusing one parameter
	// as both a bigint and part of a string leaves Postgres unable to deduce a
	// single type for it.
	key := fmt.Sprintf("execute:%d", state.MigrationID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, instance_id, weight, idempotency_key)
		VALUES ('execute', 'migration', $1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		state.MigrationID, instanceID, weight, key); err != nil {
		return fmt.Errorf("enqueue execution: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'READY_TO_EXECUTE', state_reason = NULL, updated_at = now()
		 WHERE id = $1`, requestID); err != nil {
		return fmt.Errorf("advance request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// Execution is everything needed to run one migration.
type Execution struct {
	MigrationID int64
	RequestID   int64
	DatabaseID  int64
	InstanceID  int64
	// ProjectID is carried so the executor can record activity without another
	// lookup — the instance it already joins to knows the project.
	ProjectID    int64
	DatabaseName string

	DSN  string
	From schema.Version
	To   schema.Version

	Steps []Step
}

// LoadExecution assembles the context for a migration, decrypting the target's
// credential.
//
// Unscoped: the worker acts on behalf of every project, and a job it has already
// claimed has been through the gate. Scoping here would mean the worker needed a
// project identity it has no business holding.
func (s *Store) LoadExecution(ctx context.Context, migrationID int64) (*Execution, error) {
	var x Execution
	var e endpoint
	var kind, ref string
	var ciphertext []byte
	var from, to string

	err := s.pool.QueryRow(ctx, `
		SELECT m.id, r.id, d.id, i.id, i.project_id, d.name,
		       m.from_fingerprint, m.to_fingerprint,
		       i.host, i.port, i.tls_mode, c.username, c.kind,
		       COALESCE(c.secret_ref, ''), COALESCE(c.secret_ciphertext, '\x'::bytea)
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  JOIN schemaver.credential c ON c.id = i.credential_id
		 WHERE m.id = $1 AND m.superseded_at IS NULL`, migrationID).
		Scan(&x.MigrationID, &x.RequestID, &x.DatabaseID, &x.InstanceID, &x.ProjectID,
			&x.DatabaseName,
			&from, &to, &e.host, &e.port, &e.tlsMode, &e.username, &kind, &ref, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("migration not found, or superseded since it was queued")
	}
	if err != nil {
		return nil, fmt.Errorf("load execution: %w", err)
	}
	x.From, x.To = schema.Version(from), schema.Version(to)

	if e.password, err = s.credentials(ctx, kind, ref, ciphertext); err != nil {
		return nil, err
	}
	e.database = x.DatabaseName
	x.DSN = e.dsn()

	rows, err := s.pool.Query(ctx, `
		SELECT ordinal, sql, change_id, transactional, COALESCE(note, '')
		  FROM schemaver.migration_step
		 WHERE migration_id = $1 ORDER BY ordinal`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("load steps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Ordinal, &st.SQL, &st.ChangeID,
			&st.Transactional, &st.Note); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		x.Steps = append(x.Steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(x.Steps) == 0 {
		return nil, errors.New("migration has no statements")
	}
	return &x, nil
}

// SetRequestState records where a request has got to, with the reason that
// explains it.
//
// The reason carries every distinction that did not earn a state of its own
// (D-017), so it is never optional in practice for anything but a clean
// outcome.
func (s *Store) SetRequestState(ctx context.Context, requestID int64, state, reason string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = $2, state_reason = NULLIF($3, ''), updated_at = now()
		 WHERE id = $1`, requestID, state, reason); err != nil {
		return fmt.Errorf("set request state: %w", err)
	}
	return nil
}

// Progress is one observation of a running migration, taken from outside the
// session doing the work.
type Progress struct {
	Step         int
	WaitEvent    string
	BlockedBy    []int32
	BlockerQuery string
	Phase        string
	// Percent is set only where the engine reports real progress. Index builds
	// do; table rewrites do not, and inventing a number for those would be
	// worse than admitting there isn't one.
	Percent *float64
}

// StartExecution opens an execution record and returns its id.
func (s *Store) StartExecution(ctx context.Context, migrationID int64, statements int) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO schemaver.execution (migration_id, statements_total)
		VALUES ($1, $2) RETURNING id`, migrationID, statements).Scan(&id); err != nil {
		return 0, fmt.Errorf("start execution record: %w", err)
	}
	return id, nil
}

// BeginStep records that a statement has started.
func (s *Store) BeginStep(ctx context.Context, executionID int64, ordinal int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.execution_step (execution_id, ordinal)
		VALUES ($1, $2) ON CONFLICT (execution_id, ordinal) DO NOTHING`,
		executionID, ordinal); err != nil {
		return fmt.Errorf("record step start: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.execution
		   SET current_step = $2, current_started_at = now(),
		       wait_event = NULL, blocked_by = NULL, blocker_query = NULL,
		       progress_phase = NULL, progress_percent = NULL
		 WHERE id = $1`, executionID, ordinal); err != nil {
		return fmt.Errorf("set current step: %w", err)
	}
	return tx.Commit(ctx)
}

// FinishStep records a statement's outcome. A batch that rolled back finishes
// every statement in it with the same error, because none of them took effect.
func (s *Store) FinishStep(ctx context.Context, executionID int64, ordinal int, cause error) error {
	var msg *string
	if cause != nil {
		text := cause.Error()
		msg = &text
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.execution_step
		   SET finished_at = now(), error = $3
		 WHERE execution_id = $1 AND ordinal = $2`, executionID, ordinal, msg); err != nil {
		return fmt.Errorf("record step finish: %w", err)
	}
	if cause == nil {
		if _, err := s.pool.Exec(ctx, `
			UPDATE schemaver.execution SET statements_done = statements_done + 1
			 WHERE id = $1`, executionID); err != nil {
			return fmt.Errorf("count statement: %w", err)
		}
	}
	return nil
}

// RecordProgress stores one observation. Failures are the caller's to ignore:
// losing a progress sample must never affect the migration itself.
func (s *Store) RecordProgress(ctx context.Context, executionID int64, p Progress) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE schemaver.execution
		   SET wait_event = NULLIF($2, ''), blocked_by = $3,
		       blocker_query = NULLIF($4, ''),
		       progress_phase = NULLIF($5, ''), progress_percent = $6,
		       observed_at = now()
		 WHERE id = $1 AND state = 'running'`,
		executionID, p.WaitEvent, p.BlockedBy, p.BlockerQuery, p.Phase, p.Percent)
	return err
}

// FinishExecution closes an execution record.
func (s *Store) FinishExecution(ctx context.Context, executionID int64, state, reason, final string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.execution
		   SET state = $2, reason = NULLIF($3, ''),
		       final_fingerprint = NULLIF($4, ''), finished_at = now(),
		       current_step = NULL, current_started_at = NULL
		 WHERE id = $1`, executionID, state, reason, final); err != nil {
		return fmt.Errorf("finish execution record: %w", err)
	}
	return nil
}
