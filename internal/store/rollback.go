package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// ErrNothingToRollBack is returned when the database is already where the
// migration started.
var ErrNothingToRollBack = errors.New(
	"this database is already at the schema the migration started from; there is " +
		"nothing to undo")

// ErrCannotPlace is returned when the database is somewhere the revert cannot
// start from.
var ErrCannotPlace = errors.New(
	"this database is not at the migration's target, nor at any state the proof " +
		"recorded after one of its statements, so it holds something this migration " +
		"alone did not produce; a rollback cannot be composed for it")

// Rollback describes what undoing would run.
type Rollback struct {
	MigrationID int64
	RequestID   int64
	// Applied is how many forward statements took effect, established by
	// matching the live schema against the proof's fingerprint chain.
	Applied int
	Total   int
	// Partial reports that the migration stopped part way, so only some of the
	// revert applies.
	Partial bool
	// From is where the database is now; To is where undoing would leave it.
	From, To schema.Version
	Steps    []RevertStep
	// LosesData reports that undoing restores shape without contents.
	LosesData bool
}

// PlanRollback works out what undoing this migration would mean, given where
// the database actually is.
//
// The whole question is *where it stopped*, and the fingerprint chain is what
// answers it. A database sitting at the state the proof recorded after
// statement four applied exactly four statements, so the rollback is the revert
// of those four and nothing else — dropping an index that was never created
// would fail, and failing during a rollback is the worst place to fail.
//
// Refuses rather than guesses when the live schema matches nothing in the
// chain. That means something changed outside this migration, and composing a
// rollback for a state nobody predicted is exactly the automatic recovery D-013
// refuses to do.
func (s *Scope) PlanRollback(ctx context.Context, migrationID int64) (*Rollback, error) {
	r := &Rollback{MigrationID: migrationID}

	var live, from, to string
	err := s.store.pool.QueryRow(ctx, `
		SELECT req.id, COALESCE(d.current_fingerprint, ''),
		       m.from_fingerprint, m.to_fingerprint
		  FROM schemaver.migration m
		  JOIN schemaver.change_request req ON req.id = m.change_request_id
		  JOIN schemaver.database d ON d.id = req.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE m.id = $1 AND i.project_id = ANY($2) AND m.superseded_at IS NULL`,
		migrationID, s.projects).Scan(&r.RequestID, &live, &from, &to)
	if err != nil {
		return nil, fmt.Errorf("load migration for rollback: %w", err)
	}
	r.From, r.To = schema.Version(live), schema.Version(from)

	steps, err := s.RevertSteps(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	chain, err := s.store.ExpectedAfter(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	r.Total = len(chain)

	switch {
	case live == from:
		return nil, ErrNothingToRollBack
	case live == to:
		// Everything applied: the whole revert, which is the one a reviewer
		// read in full.
		r.Applied = len(chain)
	default:
		placed := -1
		for i, fp := range chain {
			if fp != "" && string(fp) == live {
				placed = i + 1
			}
		}
		if placed < 0 {
			return nil, ErrCannotPlace
		}
		r.Applied, r.Partial = placed, true
		if !Sliceable(steps) {
			return nil, fmt.Errorf(
				"this migration stopped after statement %d, but its revert cannot be "+
					"cut down to match — some revert statements could not be matched to "+
					"the forward statement they undo, so only a rollback from the "+
					"target can be offered", placed)
		}
	}

	r.Steps = SliceRevert(steps, r.Applied)
	if len(r.Steps) == 0 {
		return nil, ErrNothingToRollBack
	}
	for _, st := range r.Steps {
		if st.StructureOnly {
			r.LosesData = true
		}
	}
	return r, nil
}

// EnqueueRollback queues the undo.
//
// The plan is recomputed here rather than trusted from the page that offered
// it: a database can move between somebody seeing the button and pressing it,
// which is the same reason the approval gate is re-evaluated at execution time.
func (s *Scope) EnqueueRollback(ctx context.Context, actorID, migrationID int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	plan, err := s.PlanRollback(ctx, migrationID)
	if err != nil {
		return err
	}

	// Keyed on the migration and where the database is, so pressing twice
	// queues once — while a rollback from a different state is a different
	// piece of work and gets its own job.
	key := fmt.Sprintf("rollback:%d:%s", migrationID, plan.From.Short())
	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, instance_id, weight, idempotency_key)
		SELECT 'rollback', 'migration', $1, d.instance_id, 8, $2
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		  JOIN schemaver.database d ON d.id = r.database_id
		 WHERE m.id = $1
		ON CONFLICT (idempotency_key) DO NOTHING`, migrationID, key); err != nil {
		return fmt.Errorf("enqueue rollback: %w", err)
	}

	what := fmt.Sprintf("undoing all %d statement(s)", plan.Applied)
	if plan.Partial {
		what = fmt.Sprintf("undoing the %d statement(s) that applied, of %d",
			plan.Applied, plan.Total)
	}
	s.record(ctx, Warn("rollback.queued", what+", returning the database to "+
		plan.To.Short()).
		By(actorID).
		OnRequest(plan.RequestID).
		OnMigration(migrationID).
		With(map[string]any{
			"applied": plan.Applied, "total": plan.Total,
			"partial": plan.Partial, "loses_data": plan.LosesData,
		}))
	return nil
}

// LoadRollbackExecution assembles the undo for the executor.
//
// The same shape as a forward execution and run by the same code. A rollback is
// not a special mode: it is a set of statements, a state they start from and a
// state they must produce, which is what an execution has always been.
func (s *Store) LoadRollbackExecution(ctx context.Context, migrationID int64) (*Execution, error) {
	x, err := s.LoadExecution(ctx, migrationID)
	if err != nil {
		return nil, err
	}

	scope := &Scope{store: s, projects: []int64{x.ProjectID}, writable: x.ProjectID}
	plan, err := scope.PlanRollback(ctx, migrationID)
	if err != nil {
		return nil, err
	}

	x.From, x.To = plan.From, plan.To
	x.Steps = x.Steps[:0]
	for _, st := range plan.Steps {
		x.Steps = append(x.Steps, Step{
			Ordinal: st.Ordinal, SQL: st.SQL, ChangeID: st.ChangeID,
			Transactional: st.Transactional, Note: st.Note,
		})
	}
	// No chain for a rollback: the proof recorded fingerprints for the forward
	// statements, and these are different statements. A half-applied rollback
	// is placed by its endpoints only, which is the position everything was in
	// before D-021.
	x.Expected = nil
	return x, nil
}
