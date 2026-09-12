// Package executor applies a migration to a database.
//
// It is the only code in schemaver that writes DDL to a database somebody
// depends on. Everything else is read-only or confined to a throwaway shadow —
// which is why the lock, the reconciliation table, the shadow proof and the
// approval gate were all built before this.
//
// The sequence matters more than any individual step, and is D-013's:
//
//	lock → read live state → verify the precondition → apply → verify the
//	outcome → record
//
// Taking the lock first is what makes the subsequent read trustworthy. The lock
// does not establish truth; it freezes truth long enough to act on it.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/guard"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// Outcome is where a migration ended up, in the vocabulary of D-017.
type Outcome struct {
	// State is one of COMPLETED, FAILED or NEEDS_ATTENTION.
	State string
	// Reason carries every distinction that did not earn a state of its own.
	Reason string
	// StepsApplied is how many statements ran, which is not the same as how many
	// took effect if one failed partway.
	StepsApplied int
	// Final is the schema the database actually ended at, established by reading
	// it rather than assumed from what we ran.
	Final schema.Version
	// Retryable reports whether trying again unchanged could succeed. A held
	// lock is retryable; a half-applied migration is not.
	Retryable bool
}

const (
	StateCompleted      = "COMPLETED"
	StateFailed         = "FAILED"
	StateNeedsAttention = "NEEDS_ATTENTION"
	StateStale          = "STALE"
)

// Executor applies migrations.
type Executor struct {
	store *store.Store
	log   *slog.Logger
	// StatementTimeout and LockTimeout bound every statement. An ALTER queued
	// behind a long read blocks every later query on that table, so failing fast
	// is the only safe default — a migration that cannot get its lock promptly
	// should give up rather than take the table down while it waits.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

func New(s *store.Store, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{
		store: s, log: log,
		StatementTimeout: 30 * time.Minute,
		LockTimeout:      10 * time.Second,
	}
}

// Execute applies a migration and reports where it ended up.
//
// It returns an Outcome rather than an error for anything that happened *to the
// database*, because "it failed" and "we could not tell what happened" need
// different responses and a bare error cannot carry that. An error is returned
// only when we could not get far enough to have an outcome at all.
func (e *Executor) Execute(ctx context.Context, x *store.Execution) (Outcome, error) {
	conn, err := pgx.Connect(ctx, x.DSN)
	if err != nil {
		return Outcome{
			State: StateFailed, Retryable: true,
			Reason: fmt.Sprintf("could not connect to %s: %v", x.DatabaseName, err),
		}, nil
	}
	defer conn.Close(context.Background())

	lock, err := guard.Acquire(ctx, conn)
	if err != nil {
		var held *guard.LockedError
		if errors.As(err, &held) {
			// Somebody else is migrating this database. Waiting would turn one
			// stuck migration into a queue of them.
			return Outcome{
				State: StateFailed, Retryable: true,
				Reason: err.Error(),
			}, nil
		}
		return Outcome{}, err
	}
	defer func() {
		if rerr := lock.Release(context.Background()); rerr != nil {
			e.log.Warn("releasing the migration lock failed", "error", rerr)
		}
	}()

	// Read live state only now that it cannot change underneath us.
	before, err := readFingerprint(ctx, conn)
	if err != nil {
		return Outcome{}, err
	}
	if before != x.From {
		// The dispatcher only claims a migration whose start matches, so this
		// means the database moved between claim and lock. Nothing has been
		// touched.
		return Outcome{
			State: StateStale, Final: before,
			Reason: fmt.Sprintf(
				"the database is at %s but this migration starts from %s; "+
					"it changed after this was queued, so nothing was applied",
				before.Short(), x.From.Short()),
		}, nil
	}

	// From here on there is something to watch, so there is something to show.
	executionID, err := e.store.StartExecution(ctx, x.MigrationID, len(x.Steps))
	if err != nil {
		return Outcome{}, err
	}
	e.event(ctx, x, executionID, store.Info("execution.started",
		fmt.Sprintf("applying %d statement(s) to %s, taking it from %s to %s",
			len(x.Steps), x.DatabaseName, x.From.Short(), x.To.Short())).
		With(map[string]any{
			"database": x.DatabaseName, "statements": len(x.Steps),
			"from": string(x.From), "to": string(x.To),
		}))

	finish := func(state, reason, final string) {
		// Written before the record is closed, so the log ends with the reason
		// the run ended rather than leaving the reader to infer it from the
		// last thing that happened to be observed.
		level := store.Info
		switch state {
		case "failed":
			level = store.Warn
		case "needs_attention":
			level = store.Error
		}
		e.event(context.WithoutCancel(ctx), x, executionID,
			level("execution."+state, reason).
				With(map[string]any{"state": state, "final_fingerprint": final}))

		if ferr := e.store.FinishExecution(context.WithoutCancel(ctx),
			executionID, state, reason, final); ferr != nil {
			e.log.Warn("closing the execution record failed", "error", ferr)
		}
	}

	// A second connection watches from outside. PostgreSQL reports nothing to
	// the session executing DDL — it is blocked inside the statement — so lock
	// waits and index-build progress have to be read by somebody else asking.
	if pid, perr := backendPID(ctx, conn); perr == nil {
		stopObserving := e.observe(x, executionID, pid)
		defer stopObserving()
	} else {
		e.log.Debug("could not determine the backend pid; progress will not be reported",
			"error", perr)
	}

	applied, runErr := e.apply(ctx, conn, executionID, x)

	after, readErr := readFingerprint(ctx, conn)
	if readErr != nil {
		finish("needs_attention", fmt.Sprintf(
			"ran %d of %d statements, then could not read the schema back: %v",
			applied, len(x.Steps), readErr), "")
		// We cannot say what state the database is in, which is the one answer
		// that must never be guessed.
		return Outcome{
			State: StateNeedsAttention, StepsApplied: applied,
			Reason: fmt.Sprintf(
				"ran %d of %d statements, then could not read the schema back (%v); "+
					"the database's state is unknown and must be established by hand",
				applied, len(x.Steps), readErr),
		}, nil
	}

	switch {
	case runErr != nil && after == x.From:
		// Rolled back cleanly. The database is exactly where it started.
		finish("failed", runErr.Error(), string(after))
		return Outcome{
			State: StateFailed, StepsApplied: applied, Final: after, Retryable: true,
			Reason: fmt.Sprintf("statement %d failed and rolled back; the database is unchanged: %v",
				applied+1, runErr),
		}, nil

	case runErr != nil:
		// Partly applied. Never resolved automatically (D-013): rolling forward
		// or back on its own turns a contained problem into an incident.
		finish("needs_attention", runErr.Error(), string(after))
		return Outcome{
			State: StateNeedsAttention, StepsApplied: applied, Final: after,
			Reason: fmt.Sprintf(
				"statement %d failed after %d had already applied. %s A human must "+
					"decide whether to roll forward or back. Cause: %v",
				applied+1, applied, placement(x, after, applied), runErr),
		}, nil

	case after != x.To:
		// Everything ran and the result is wrong. Every upstream gate passed —
		// the shadow proof, the precondition — so either something changed out
		// of band mid-execution or an assumption is broken.
		finish("needs_attention", fmt.Sprintf(
			"every statement succeeded but the schema is %s, not the declared %s",
			after.Short(), x.To.Short()), string(after))
		return Outcome{
			State: StateNeedsAttention, StepsApplied: applied, Final: after,
			Reason: fmt.Sprintf(
				"every statement succeeded but the database is at %s, not the declared "+
					"target %s. %s Do not retry until this is understood",
				after.Short(), x.To.Short(), provenance(x)),
		}, nil
	}

	if err := recordApplied(ctx, conn, x, applied); err != nil {
		// The migration worked; only our note about it failed. Say so rather
		// than implying the schema change did not happen.
		e.log.Warn("could not write the history row in the target database",
			"database", x.DatabaseName, "error", err)
		e.event(ctx, x, executionID, store.Warn("history.unwritten",
			"the migration applied, but schemaver could not record it inside the "+
				"target database: "+err.Error()))
	}

	finish("completed", fmt.Sprintf("applied %d statement(s)", applied), string(after))
	return Outcome{
		State: StateCompleted, StepsApplied: applied, Final: after,
		Reason: fmt.Sprintf("applied %d statement(s); the database is at %s",
			applied, after.Short()),
	}, nil
}

// apply runs the statements, returning how many completed.
//
// Consecutive transactional statements share one transaction, so that as much of
// the migration as possible is atomic. A statement that refuses to run in a
// transaction — a concurrent index build — breaks the batch and runs alone,
// which is exactly the atomicity gap D-006 requires review to show.
func (e *Executor) apply(ctx context.Context, conn *pgx.Conn, executionID int64, x *store.Execution) (int, error) {
	applied := 0

	for i := 0; i < len(x.Steps); {
		if !x.Steps[i].Transactional {
			st := x.Steps[i]
			e.mark(ctx, x, executionID, st)
			err := e.runStandalone(ctx, conn, st)
			e.markDone(ctx, executionID, st.Ordinal, err)
			if err != nil {
				e.event(ctx, x, executionID,
					store.Error("step.failed", err.Error()).At(st.Ordinal))
				return applied, err
			}
			e.event(ctx, x, executionID,
				store.Info("step.applied", "applied "+st.ChangeID).At(st.Ordinal))
			applied++
			i++
			continue
		}

		// Gather the run of transactional statements starting here.
		j := i
		for j < len(x.Steps) && x.Steps[j].Transactional {
			j++
		}
		n, err := e.runBatch(ctx, conn, x, executionID, x.Steps[i:j])
		applied += n
		if err != nil {
			return applied, err
		}
		i = j
	}
	return applied, nil
}

// mark and markDone keep the progress record current. They never return an
// error: losing a progress update must not affect the migration, and a caller
// forced to handle that error would have nothing useful to do with it.
func (e *Executor) mark(ctx context.Context, x *store.Execution, executionID int64, st store.Step) {
	if err := e.store.BeginStep(ctx, executionID, st.Ordinal); err != nil {
		e.log.Debug("recording step start failed", "step", st.Ordinal, "error", err)
	}
	e.event(ctx, x, executionID, store.Info("step.started", "running "+st.ChangeID).At(st.Ordinal))
}

func (e *Executor) markDone(ctx context.Context, executionID int64, ordinal int, cause error) {
	if err := e.store.FinishStep(context.WithoutCancel(ctx), executionID, ordinal, cause); err != nil {
		e.log.Debug("recording step finish failed", "step", ordinal, "error", err)
	}
}

// event adds one entry to the activity log, tagged with every entity the run
// concerns so it shows up on the request's timeline and the database's as well
// as the execution's. Like mark, it returns nothing: failing to describe a
// migration must not affect the migration.
func (e *Executor) event(ctx context.Context, x *store.Execution, executionID int64, ev *store.Event) {
	tagged := ev.
		OnExecution(executionID).
		OnMigration(x.MigrationID).
		OnRequest(x.RequestID).
		OnDatabase(x.DatabaseID)
	tagged.InstanceID = &x.InstanceID
	if err := e.store.Record(ctx, x.ProjectID, tagged); err != nil {
		e.log.Debug("recording an execution event failed", "error", err)
	}
}

func (e *Executor) runBatch(ctx context.Context, conn *pgx.Conn, x *store.Execution, executionID int64, steps []store.Step) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	if err := e.applyTimeouts(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return 0, err
	}

	for _, st := range steps {
		e.log.Info("applying", "step", st.Ordinal, "change", st.ChangeID)
		e.mark(ctx, x, executionID, st)
		if _, err := tx.Exec(ctx, st.SQL); err != nil {
			_ = tx.Rollback(ctx)
			failure := fmt.Errorf("step %d (%s): %w", st.Ordinal, st.ChangeID, err)
			// Nothing in the batch took effect, so every statement in it failed
			// — including the ones that had appeared to succeed.
			for _, rolled := range steps {
				if rolled.Ordinal > st.Ordinal {
					continue
				}
				e.markDone(ctx, executionID, rolled.Ordinal, failure)
				// The log distinguishes the statement that actually broke from
				// the ones undone alongside it. The step records cannot — they
				// all carry the same cause, because none of them took effect —
				// but a reader looking for what went wrong needs to be pointed
				// at one statement rather than all of them.
				if rolled.Ordinal == st.Ordinal {
					e.event(ctx, x, executionID,
						store.Error("step.failed", err.Error()).At(rolled.Ordinal))
				} else {
					e.event(ctx, x, executionID,
						store.Warn("step.rolled_back",
							"undone when a later statement in the same transaction failed").
							At(rolled.Ordinal))
				}
			}
			return 0, failure
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	for _, st := range steps {
		e.markDone(ctx, executionID, st.Ordinal, nil)
		e.event(ctx, x, executionID,
			store.Info("step.applied", "applied "+st.ChangeID).At(st.Ordinal))
	}
	return len(steps), nil
}

// runStandalone executes a statement that cannot be wrapped in a transaction.
func (e *Executor) runStandalone(ctx context.Context, conn *pgx.Conn, st store.Step) error {
	e.log.Info("applying outside a transaction", "step", st.Ordinal, "change", st.ChangeID)
	// A concurrent index build must not be cut short by a statement timeout: it
	// is expected to take a long time, and being killed leaves an invalid index
	// behind. The lock timeout still applies.
	if _, err := conn.Exec(ctx, "SET statement_timeout = 0"); err != nil {
		return fmt.Errorf("clear statement timeout: %w", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d",
		e.LockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set lock timeout: %w", err)
	}
	if _, err := conn.Exec(ctx, st.SQL); err != nil {
		return fmt.Errorf("step %d (%s): %w", st.Ordinal, st.ChangeID, err)
	}
	return nil
}

// applyTimeouts bounds a transaction's statements.
//
// lock_timeout is the important one. An ALTER waiting for a lock queues behind
// existing readers and then blocks every subsequent query on that table, so a
// migration that cannot get its lock promptly takes the table down while it
// waits. Failing fast is the only safe default.
func (e *Executor) applyTimeouts(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d",
		e.LockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d",
		e.StatementTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set statement timeout: %w", err)
	}
	return nil
}

func readFingerprint(ctx context.Context, conn *pgx.Conn) (schema.Version, error) {
	s, err := introspect.Schema(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("read schema: %w", err)
	}
	return schema.Fingerprint(s)
}

// historyDDL creates the tracking table inside the managed database.
//
// It lives there, not only in our own store, so the database stays
// self-describing: lose the control plane entirely and each database still
// reports its own version, and a database restored from backup reveals its true
// state rather than the one we remember.
const historyDDL = `
CREATE SCHEMA IF NOT EXISTS schemaver_history;
CREATE TABLE IF NOT EXISTS schemaver_history.applied (
    to_fingerprint   text PRIMARY KEY,
    from_fingerprint text        NOT NULL,
    statements       integer     NOT NULL,
    applied_at       timestamptz NOT NULL DEFAULT now()
);`

func recordApplied(ctx context.Context, conn *pgx.Conn, x *store.Execution, statements int) error {
	if _, err := conn.Exec(ctx, historyDDL); err != nil {
		return fmt.Errorf("create history table: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO schemaver_history.applied
		    (to_fingerprint, from_fingerprint, statements)
		VALUES ($1, $2, $3)
		ON CONFLICT (to_fingerprint) DO UPDATE
		   SET from_fingerprint = EXCLUDED.from_fingerprint,
		       statements = EXCLUDED.statements,
		       applied_at = now()`,
		string(x.To), string(x.From), statements); err != nil {
		return fmt.Errorf("record applied migration: %w", err)
	}
	return nil
}

// Summarise renders an outcome for a log line.
func (o Outcome) Summarise() string {
	return strings.Join([]string{o.State, o.Reason}, ": ")
}

// placement says where a half-applied migration actually stopped.
//
// The proof recorded the fingerprint each statement should leave behind, so a
// database sitting at one of those values places the failure exactly: these
// statements took effect and those did not. Without a proof the best that can
// be said is that it is somewhere between the start and the target, which is
// the difference between a recoverable incident and an investigation.
func placement(x *store.Execution, after schema.Version, applied int) string {
	for i, want := range x.Expected {
		if want != "" && want == after {
			return fmt.Sprintf(
				"The database is at %s, which the proof recorded as the state after "+
					"statement %d — so statements 1 to %d took effect and %d to %d did not.",
				after.Short(), i+1, i+1, i+2, len(x.Steps))
		}
	}
	if after == x.From {
		return fmt.Sprintf(
			"The database is back at its start (%s), so nothing took effect.",
			x.From.Short())
	}
	if len(x.Expected) == 0 {
		return fmt.Sprintf(
			"The database is at %s — neither its start (%s) nor its target (%s). "+
				"This migration was never proven against a throwaway copy, so there "+
				"is no record of what each statement should have left behind and the "+
				"failure cannot be placed more precisely.",
			after.Short(), x.From.Short(), x.To.Short())
	}
	return fmt.Sprintf(
		"The database is at %s, which matches neither its start (%s), its target "+
			"(%s), nor any state the proof recorded after a statement — so it holds "+
			"something this migration alone did not produce.",
		after.Short(), x.From.Short(), x.To.Short())
}

// provenance says whether the migration had been proven, when every statement
// applied and the result was still wrong.
//
// This message is read at the worst moment there is, and it used to assert that
// a shadow proof had passed whether or not one had ever run. An unproven
// migration reaching a wrong schema has an ordinary explanation; a proven one
// does not, and the reader needs to know which of those they are looking at.
func provenance(x *store.Execution) string {
	if len(x.Expected) == 0 {
		return "This migration was never proven against a throwaway copy, so this " +
			"may simply be a change schemaver cannot express as SQL — look for a " +
			"statement rendered as a comment."
	}
	return "This migration was proven against a throwaway copy built at its " +
		"starting point, so either something changed outside schemaver while it " +
		"ran, or an assumption is wrong."
}
