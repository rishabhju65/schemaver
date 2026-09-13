package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/shadow"
)

// branchWrite applies somebody's DDL to a branch and records what it did.
//
// A branch has no database, so the statements are run against a throwaway one
// built at the branch's current schema — the same machinery the rehearsal and
// authored changes use. That is what makes a branch head a schema that
// genuinely exists: PostgreSQL says whether the statements work and what they
// produce, rather than schemaver predicting it from the text.
//
// It is also the first and only thing that can say the statements are wrong. A
// syntax error, a column already there, a type that will not cast: all of it
// surfaces here, against a copy, and goes back to the author as the branch's
// write error.
func (w *Worker) branchWrite(ctx context.Context, branchID int64) error {
	task, err := w.store.LoadBranchWrite(ctx, branchID)
	if err != nil {
		// Closed, or the write was superseded while this sat in the queue.
		w.log.Info("nothing to write", "branch", branchID, "reason", err)
		return nil
	}

	if w.cfg.Shadow == nil {
		return w.store.FailBranchWrite(ctx, branchID,
			"no shadow server is configured, so what these statements do cannot "+
				"be worked out; a branch cannot take a change without one")
	}

	// An empty target, because nobody knows where these statements land — that
	// is the whole question. Prove reports what it actually reached.
	proof, err := w.cfg.Shadow.Prove(ctx, task.BaseDDL, task.Statements, nil, task.Head, "")

	var badBase *shadow.BaseError
	var badStep *shadow.StepError
	var mismatch *shadow.MismatchError
	switch {
	case errors.As(err, &badBase):
		// schemaver could not rebuild a schema it had already stored, which is
		// its own defect rather than anything about what was written.
		return w.store.FailBranchWrite(ctx, branchID, badBase.Error())

	case errors.As(err, &badStep):
		w.log.Warn("branch statements do not apply", "branch", branchID,
			"statement", badStep.Ordinal, "error", badStep.Err)
		return w.store.FailBranchWrite(ctx, branchID, badStep.Error())

	case errors.As(err, &mismatch):
		// Expected: an empty target cannot be matched, and what was reached is
		// the answer being asked for.

	case err != nil:
		return w.store.FailBranchWrite(ctx, branchID,
			fmt.Sprintf("these statements could not be tried: %v", err))
	}

	if proof.Final == task.Head {
		return w.store.FailBranchWrite(ctx, branchID,
			"these statements ran but left the schema exactly as it was, so "+
				"there is nothing for the branch to take on")
	}

	// Prove kept the schema it produced, so the branch's new head costs no
	// second shadow: rebuilding one to read the same result again would double
	// the cost of every write.
	sch := proof.FinalSchema
	if sch == nil {
		return w.store.FailBranchWrite(ctx, branchID,
			"the statements applied but the resulting schema could not be read back")
	}
	changes := diff.Compute(task.BaseSchema, sch).Changes

	w.log.Info("branch took a change", "branch", branchID, "name", task.Name,
		"from", task.Head.Short(), "to", proof.Final.Short(), "changes", len(changes))
	return w.store.RecordBranchWrite(ctx, task, proof.Final, sch, changes)
}
