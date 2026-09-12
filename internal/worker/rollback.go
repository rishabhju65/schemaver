package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/executor"
	"github.com/rishabhju65/schemaver/internal/store"
)

// rollback undoes a migration, in whole or in the part of it that applied.
//
// Run by the same executor as the forward migration, because it is the same
// kind of work: a set of statements, a state they start from, and a state they
// must produce. The lock, the precondition and the verification are all wanted
// here for the same reasons they are wanted going forward — more so, since this
// runs after something has already gone wrong.
func (w *Worker) rollback(ctx context.Context, migrationID int64) error {
	x, err := w.store.LoadRollbackExecution(ctx, migrationID)
	switch {
	case errors.Is(err, store.ErrNothingToRollBack):
		// The database moved back on its own, or somebody got there first.
		// Nothing to do and nothing wrong.
		w.log.Info("rollback no longer needed", "migration", migrationID)
		return nil
	case errors.Is(err, store.ErrCannotPlace):
		// Refused rather than guessed. Composing a rollback for a state nobody
		// predicted is the automatic recovery D-013 exists to refuse.
		w.log.Error("ROLLBACK REFUSED — the database is somewhere this migration "+
			"alone did not produce", "migration", migrationID)
		return err
	case err != nil:
		return err
	}

	w.log.Warn("rolling back", "migration", migrationID,
		"database", x.DatabaseName, "statements", len(x.Steps),
		"from", x.From.Short(), "to", x.To.Short())

	if serr := w.store.SetRequestState(ctx, x.RequestID, "EXECUTING",
		fmt.Sprintf("undoing %d statement(s) on %s", len(x.Steps), x.DatabaseName)); serr != nil {
		w.log.Warn("marking the request as rolling back failed", "error", serr)
	}

	outcome, err := w.exec.Execute(ctx, x)
	if err != nil {
		_ = w.store.SetRequestState(context.WithoutCancel(ctx), x.RequestID,
			executor.StateNeedsAttention,
			fmt.Sprintf("the rollback could not be carried out: %v", err))
		return err
	}

	// A completed rollback closes the request rather than completing it. The
	// change did not happen: saying COMPLETED would record the opposite of what
	// took place, and leaving it open would invite somebody to press execute
	// again on a migration that has just been undone.
	state, reason := outcome.State, outcome.Reason
	if outcome.State == executor.StateCompleted {
		state = "CLOSED"
		reason = "rolled back; the database is at " + outcome.Final.Short()
	}
	if serr := w.store.SetRequestState(context.WithoutCancel(ctx), x.RequestID,
		state, reason); serr != nil {
		w.log.Error("recording the rollback outcome failed",
			"migration", migrationID, "error", serr)
	}

	// The schema has moved; read it back so everything that consults the
	// recorded fingerprint is consulting this one and not the previous.
	if oerr := w.store.ObserveNow(context.WithoutCancel(ctx), x.DatabaseID); oerr != nil {
		w.log.Warn("could not queue a read after the change", "error", oerr)
	}

	switch outcome.State {
	case executor.StateCompleted:
		w.log.Info("rolled back", "migration", migrationID,
			"database", x.DatabaseName, "version", outcome.Final.Short())
	case executor.StateNeedsAttention:
		w.log.Error("ROLLBACK HALTED — a human must decide",
			"migration", migrationID, "database", x.DatabaseName,
			"detail", outcome.Reason)
	default:
		w.log.Warn("rollback did not apply", "migration", migrationID,
			"state", outcome.State, "detail", outcome.Reason)
	}

	if outcome.Retryable {
		return errors.New(outcome.Reason)
	}
	return nil
}
