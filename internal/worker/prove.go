package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/shadow"
	"github.com/rishabhju65/schemaver/internal/store"
)

// prove applies a migration to a throwaway database built at its starting
// point, and records whether it produced the schema it declares.
//
// Every outcome is written down, including the ones that are schemaver's fault
// rather than the migration's. The alternative — failing the job and retrying —
// would leave the request sitting in STAGE_SANITY with nothing said, and a
// request that never moves is the one failure mode a reviewer cannot diagnose.
func (w *Worker) prove(ctx context.Context, migrationID int64) error {
	task, err := w.store.LoadProofTask(ctx, migrationID)
	if errors.Is(err, store.ErrNoProofNeeded) {
		// Superseded or closed while queued. Nothing to prove and nothing wrong.
		return nil
	}
	if err != nil {
		return err
	}

	if w.cfg.Shadow == nil {
		if err := w.store.RecordRevertProof(ctx, migrationID, "unproven",
			"no shadow server is configured"); err != nil {
			return err
		}
		return w.store.RecordProof(ctx, migrationID, "unproven",
			"no shadow server is configured, so this migration has not been "+
				"checked against a throwaway copy of the database", nil)
	}

	// The starting schema is rebuilt from the stored blob rather than dumped
	// from the real database. The blob is what the migration was generated
	// against; reading the database again could prove a different starting
	// point than the one the migration declares, which would make the proof
	// answer a question nobody asked.
	proof, err := w.cfg.Shadow.Prove(ctx, task.BaseDDL, task.Statements,
		task.Revert, task.From, task.To)

	var badBase *shadow.BaseError
	var badStep *shadow.StepError
	var mismatch *shadow.MismatchError
	var badRevert *shadow.RevertError
	switch {
	case err == nil:
		w.log.Info("migration proved", "migration", migrationID,
			"statements", len(task.Statements), "revert", len(task.Revert),
			"from", task.From.Short(), "to", task.To.Short())
		if err := w.store.RecordRevertProof(ctx, migrationID, revertVerdict(task), ""); err != nil {
			return err
		}
		return w.store.RecordProof(ctx, migrationID, "passed", "", proof.After)

	case errors.As(err, &badRevert):
		// The forward half is proven — it ran and verified before the revert
		// was attempted — so it is recorded as passing. Only the way back
		// failed, and saying otherwise would send somebody to change statements
		// that are correct.
		w.log.Warn("the revert does not lead back", "migration", migrationID,
			"error", badRevert)
		if err := w.store.RecordRevertProof(ctx, migrationID, "failed", badRevert.Error()); err != nil {
			return err
		}
		return w.store.RecordProof(ctx, migrationID, "passed", "", proof.After)

	case errors.As(err, &badBase):
		// Not the migration's fault, so not recorded against it as a failure.
		// Somebody sent to rewrite a migration that was never the problem
		// changes something that was correct.
		return w.recordProblem(ctx, migrationID, badBase.Error())

	case errors.As(err, &badStep):
		w.log.Warn("migration failed its proof", "migration", migrationID,
			"statement", badStep.Ordinal, "error", badStep.Err)
		return w.store.RecordProof(ctx, migrationID, "failed", badStep.Error(),
			partial(proof))

	case errors.As(err, &mismatch):
		w.log.Warn("migration produced the wrong schema", "migration", migrationID,
			"got", mismatch.Got.Short(), "want", mismatch.Want.Short())
		return w.store.RecordProof(ctx, migrationID, "failed", fmt.Sprintf(
			"every statement applied, but the result was %s rather than the "+
				"declared %s. Something in this change cannot be expressed as SQL "+
				"schemaver knows how to write — look for a statement rendered as a "+
				"comment", mismatch.Got.Short(), mismatch.Want.Short()),
			partial(proof))

	default:
		return w.recordProblem(ctx, migrationID,
			fmt.Sprintf("the proof could not be run: %v", err))
	}
}

// recordProblem marks a migration unproven because schemaver could not run the
// check, as distinct from running it and finding the migration wanting.
func (w *Worker) recordProblem(ctx context.Context, migrationID int64, reason string) error {
	w.log.Warn("could not prove migration", "migration", migrationID, "reason", reason)
	return w.store.RecordProof(ctx, migrationID, "unproven", reason, nil)
}

// partial returns whatever the chain got to before it stopped, so a failed
// proof still says how far the migration would have got.
func partial(p *shadow.Proof) []schema.Version {
	if p == nil {
		return nil
	}
	return p.After
}

// revertVerdict reports what a clean proof establishes about the revert.
//
// A migration with no revert statements is not a migration whose revert was
// proven: there was nothing to prove. Recording "passed" for it would claim a
// check that never ran, which is the habit D-021 exists to have broken.
func revertVerdict(task *store.ProofTask) string {
	if len(task.Revert) == 0 {
		return "unproven"
	}
	return "passed"
}
