// Package guard enforces that at most one migration touches a database at a
// time, and that what it finds there is what it expected.
//
// Two mechanisms, answering two different questions (D-013):
//
//   - The advisory lock answers "is anything executing right now". Only the
//     engine knows which sessions are alive, so nothing else can answer it.
//   - Reconciliation answers "did the previous migration finish". The lock is
//     useless here: a worker that dies mid-statement has its lock released
//     immediately while the database sits half migrated.
//
// The lock is taken first. It does not establish truth — it freezes truth long
// enough to read and act on it.
package guard

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/plan"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Advisory lock coordinates. Postgres scopes advisory locks per database, so one
// fixed pair suffices — the lock in database A cannot collide with the same lock
// in database B. The two-integer form is used so the lock is identifiable in
// pg_locks by classid and objid.
const (
	lockClassID = 0x5C4EA // "schemaver"
	lockObjID   = 1       // whole-database migration lock
)

// Holder describes the session holding the migration lock.
//
// Fields may be empty when the connecting role lacks the privilege to see other
// sessions' details: pg_stat_activity hides query text from unprivileged roles.
// A partial answer is reported rather than none, because knowing a pid is still
// better than knowing nothing.
type Holder struct {
	PID        int
	State      string
	Query      string
	Running    time.Duration
	WaitEvent  string
	BlockedBy  []int32
	Privileged bool
}

// LockedError reports that another session holds the migration lock.
type LockedError struct{ Holder *Holder }

func (e *LockedError) Error() string {
	if e.Holder == nil {
		return "another session holds the migration lock on this database"
	}
	msg := fmt.Sprintf("migration lock held by pid %d for %s",
		e.Holder.PID, e.Holder.Running.Round(time.Second))
	if e.Holder.WaitEvent != "" {
		msg += fmt.Sprintf(", waiting on %s", e.Holder.WaitEvent)
	}
	if len(e.Holder.BlockedBy) > 0 {
		msg += fmt.Sprintf(", blocked by %v", e.Holder.BlockedBy)
	}
	return msg
}

// Lock is a held migration lock.
//
// It is bound to the connection that took it: the lock's lifetime is that
// connection's session, so if the process dies the engine releases it
// immediately. That is the property a lock table cannot provide, and why one is
// not used here.
type Lock struct {
	conn *pgx.Conn
}

// Acquire takes the migration lock without waiting.
//
// It never blocks. Waiting on a migration lock turns one stuck migration into a
// queue of stuck migrations, so a held lock is reported — with its holder — for
// a human or a retry policy to act on.
func Acquire(ctx context.Context, conn *pgx.Conn) (*Lock, error) {
	var got bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1, $2)`, lockClassID, lockObjID).Scan(&got); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	if !got {
		holder, err := describeHolder(ctx, conn)
		if err != nil {
			// Failing to describe the holder must not mask the fact that the
			// lock is held.
			return nil, &LockedError{}
		}
		return nil, &LockedError{Holder: holder}
	}
	return &Lock{conn: conn}, nil
}

// Release drops the lock. Closing the connection releases it too; this is for
// the case where the connection is reused.
func (l *Lock) Release(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	var released bool
	if err := l.conn.QueryRow(ctx,
		`SELECT pg_advisory_unlock($1, $2)`, lockClassID, lockObjID).Scan(&released); err != nil {
		return fmt.Errorf("release migration lock: %w", err)
	}
	l.conn = nil
	return nil
}

// describeHolder identifies the session holding the lock, so a refusal can name
// what it is waiting for rather than only that it is waiting.
func describeHolder(ctx context.Context, conn *pgx.Conn) (*Holder, error) {
	var h Holder
	var seconds *float64
	err := conn.QueryRow(ctx, `
		SELECT a.pid,
		       COALESCE(a.state, ''),
		       COALESCE(a.query, ''),
		       EXTRACT(EPOCH FROM (now() - COALESCE(a.query_start, a.backend_start))),
		       COALESCE(a.wait_event_type || ':' || a.wait_event, ''),
		       COALESCE(pg_blocking_pids(a.pid), '{}')
		  FROM pg_locks l
		  JOIN pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory'
		   AND l.classid = $1 AND l.objid = $2 AND l.objsubid = 2
		   AND l.granted
		 LIMIT 1`, lockClassID, lockObjID).
		Scan(&h.PID, &h.State, &h.Query, &seconds, &h.WaitEvent, &h.BlockedBy)
	if err != nil {
		return nil, err
	}
	if seconds != nil {
		h.Running = time.Duration(*seconds * float64(time.Second))
	}
	h.Privileged = h.Query != ""
	return &h, nil
}

// Recorded is what our own metadata believes about the previous migration.
type Recorded string

const (
	RecordedNone      Recorded = "none"
	RecordedCompleted Recorded = "completed"
	RecordedRunning   Recorded = "running"
	RecordedFailed    Recorded = "failed"
)

// Verdict is the reconciliation outcome.
type Verdict string

const (
	// Proceed: the previous migration finished and the database agrees.
	Proceed Verdict = "proceed"
	// SelfHeal: the migration completed but we died before recording it. Safe to
	// correct the record and continue.
	SelfHeal Verdict = "self_heal"
	// NothingApplied: the previous attempt left no trace; the database is still
	// at its starting point.
	NothingApplied Verdict = "nothing_applied"
	// HaltPartial: some steps applied and some did not. A human chooses whether
	// to roll forward or back.
	HaltPartial Verdict = "halt_partial"
	// HaltDrifted: we recorded completion but the database no longer matches,
	// so something changed out of band.
	HaltDrifted Verdict = "halt_drifted"
	// HaltUnknown: the live schema matches no point on the chain at all.
	HaltUnknown Verdict = "halt_unknown"
)

// Outcome is the result of reconciling recorded state against live state.
type Outcome struct {
	Verdict      Verdict
	Live         schema.Version
	StepsApplied int
	Reason       string
}

// Safe reports whether a new migration may proceed. Everything ambiguous is
// unsafe by construction: no verdict that halts can be overridden here.
func (o Outcome) Safe() bool {
	return o.Verdict == Proceed || o.Verdict == SelfHeal || o.Verdict == NothingApplied
}

// Classify decides what the live schema means, given the previous migration and
// what our records claim about it.
//
// It is a pure function of its inputs so the decision table is testable without
// a database — this is the logic that decides whether to touch production, and
// it should not require one to exercise.
func Classify(live schema.Version, prev *plan.Migration, recorded Recorded) Outcome {
	out := Outcome{Live: live}

	if prev == nil || recorded == RecordedNone {
		out.Verdict = Proceed
		out.Reason = "no previous migration to reconcile against"
		return out
	}

	// Safety is decided by the endpoints alone. The step chain is diagnostic: it
	// says *where* a migration stopped, but it never turns a halt into a
	// proceed, so a migration without per-step checkpoints is reconciled just as
	// safely — only less informatively.
	switch {
	case live == prev.To:
		out.StepsApplied = len(prev.Steps)
		if recorded == RecordedCompleted {
			out.Verdict = Proceed
			out.Reason = "previous migration completed and the database agrees"
		} else {
			out.Verdict = SelfHeal
			out.Reason = fmt.Sprintf(
				"migration %q is recorded as %s but the database is at its target %s; "+
					"it completed before the record was written",
				prev.Name, recorded, prev.To.Short())
		}

	case live == prev.From:
		if recorded == RecordedCompleted {
			// We said it finished; the database says it never happened.
			out.Verdict = HaltDrifted
			out.Reason = fmt.Sprintf(
				"migration %q is recorded as completed but the database is still at %s; "+
					"something reverted it outside schemaver",
				prev.Name, prev.From.Short())
		} else {
			out.Verdict = NothingApplied
			out.Reason = fmt.Sprintf(
				"migration %q left no trace; the database is still at %s",
				prev.Name, prev.From.Short())
		}

	default:
		// Neither endpoint. Checkpoints, when present, turn "somewhere unknown"
		// into "after step N", which is the difference between an investigation
		// and a decision.
		if steps, onChain := prev.StepsCompleted(live); onChain && prev.Checkpointed() {
			out.Verdict = HaltPartial
			out.StepsApplied = steps
			out.Reason = fmt.Sprintf(
				"migration %q stopped after step %d of %d; the database is at %s, "+
					"neither its start (%s) nor its target (%s)",
				prev.Name, steps, len(prev.Steps), live.Short(),
				prev.From.Short(), prev.To.Short())
			break
		}
		out.Verdict = HaltUnknown
		out.Reason = fmt.Sprintf(
			"live schema %s is neither the start (%s) nor the target (%s) of "+
				"migration %q, and matches no recorded checkpoint between them",
			live.Short(), prev.From.Short(), prev.To.Short(), prev.Name)
	}
	return out
}

// Reconcile takes the live schema and classifies it. The caller must already
// hold the lock — reading state before locking it yields an answer that is
// already stale (D-013).
func Reconcile(ctx context.Context, conn *pgx.Conn, prev *plan.Migration, recorded Recorded,
	read func(context.Context, *pgx.Conn) (schema.Version, error),
) (Outcome, error) {
	live, err := read(ctx, conn)
	if err != nil {
		return Outcome{}, fmt.Errorf("read live schema for reconciliation: %w", err)
	}
	return Classify(live, prev, recorded), nil
}
