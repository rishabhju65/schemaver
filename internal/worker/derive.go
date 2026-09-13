package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/shadow"
)

// derive works out what somebody's written statements actually do.
//
// The target fingerprint is produced by running them, never by asking. A
// throwaway database is built at the schema last observed on the target, the
// statements are applied to it, and what comes out is the migration's
// destination — so a written change is pinned to a real outcome exactly as a
// generated one is, and the diff between the two ends classifies it so review
// shows destructive statements marked.
//
// This is also the first thing that ever runs the statements, which makes it
// the first thing that can say they do not work. A syntax error, a table that
// does not exist, a column already present: all of it surfaces here, against a
// copy, before anybody is asked to read the change.
func (w *Worker) derive(ctx context.Context, requestID int64) error {
	task, err := w.store.LoadAuthoredTask(ctx, requestID)
	if err != nil {
		// Closed or completed while queued, or no longer an authored request.
		w.log.Info("nothing to derive", "request", requestID, "reason", err)
		return nil
	}

	if w.cfg.Shadow == nil {
		return w.store.FailDerivation(ctx, requestID,
			"no shadow server is configured, so what these statements do cannot "+
				"be worked out; a written change cannot be reviewed without it")
	}

	from := schema.Version(task.From)
	proof, err := w.cfg.Shadow.Prove(ctx, task.BaseDDL, task.Statements, nil, from, "")

	var badBase *shadow.BaseError
	var badStep *shadow.StepError
	var mismatch *shadow.MismatchError
	switch {
	case errors.As(err, &badBase):
		// schemaver could not rebuild a schema it had already read, which is its
		// own defect rather than anything about what was written.
		return w.store.FailDerivation(ctx, requestID, badBase.Error())

	case errors.As(err, &badStep):
		w.log.Warn("written statements do not apply", "request", requestID,
			"statement", badStep.Ordinal, "error", badStep.Err)
		return w.store.FailDerivation(ctx, requestID, badStep.Error())

	case errors.As(err, &mismatch):
		// Expected. Prove was asked to reach an empty target because nobody
		// knows the target yet — that is the whole point — so it reports what it
		// actually reached, and that is the answer.

	case err != nil:
		return w.store.FailDerivation(ctx, requestID,
			fmt.Sprintf("these statements could not be tried: %v", err))
	}

	to := proof.Final
	if to == from {
		return w.store.FailDerivation(ctx, requestID,
			"these statements ran but left the schema exactly as it was, so there "+
				"is nothing to migrate; a change that alters only data belongs in a "+
				"migration alongside one that alters the schema")
	}

	// The schema the statements produced, read while the shadow was still
	// standing rather than by building it again. No database has ever been
	// observed at it, so it has to be stored before a migration can reference
	// it.
	after := proof.FinalSchema
	if after == nil {
		return w.store.FailDerivation(ctx, requestID,
			"these statements produced no readable schema")
	}
	if err := w.store.StoreSchema(ctx, after, to); err != nil {
		return fmt.Errorf("store the resulting schema: %w", err)
	}
	result := diff.Compute(task.BaseSchema, after)

	migrationID, err := w.store.RecordDerivation(ctx, requestID, from, to,
		result, task.Statements, diff.Weight(result.Changes))
	if err != nil {
		return err
	}
	w.log.Info("derived a written change", "request", requestID,
		"migration", migrationID, "statements", len(task.Statements),
		"from", from.Short(), "to", to.Short(), "changes", len(result.Changes))
	return nil
}
