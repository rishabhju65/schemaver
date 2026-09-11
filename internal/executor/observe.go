package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/store"
)

// observe watches a running migration from a second connection and records what
// it sees, until the returned function is called.
//
// A second connection is not an optimisation, it is the only way. PostgreSQL
// reports nothing to the session executing DDL — that session is blocked inside
// the statement — so progress and lock waits have to be read from outside by
// somebody asking about it.
//
// Nothing here can fail the migration. A lost sample, a refused connection, a
// privilege we do not have: all of them mean less is known, never that less is
// done.
func (e *Executor) observe(dsn string, executionID int64, pid int32) func() {
	done := make(chan struct{})
	go func() {
		// Its own context, not the migration's: observation should keep running
		// while the migration does, and stop when told rather than when the
		// executing statement is cancelled.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-done
			cancel()
		}()

		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			e.log.Debug("could not open an observing connection; progress will not be reported",
				"error", err)
			return
		}
		defer conn.Close(context.Background())

		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()

		// The previous observation, so the log can record transitions rather
		// than samples. Two seconds of ticking over forty minutes is around
		// twelve hundred observations, nearly all identical; writing each one
		// would bury the handful of moments that explain the run.
		var prev store.Progress
		var waitSince time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				p, err := sample(ctx, conn, pid)
				if err != nil {
					continue
				}
				if err := e.store.RecordProgress(ctx, executionID, *p); err != nil {
					e.log.Debug("recording progress failed", "error", err)
				}
				e.logTransitions(ctx, executionID, prev, *p, &waitSince)
				prev = *p
			}
		}
	}()
	return func() { close(done) }
}

// sample reads one observation of the executing backend.
//
// The blocker's query text is available only to a role permitted to see other
// sessions' details. Without that privilege the pids still come through, which
// is the part that matters — a pid is actionable, and its query is a
// convenience.
func sample(ctx context.Context, conn *pgx.Conn, pid int32) (*store.Progress, error) {
	var p store.Progress
	var percent *float64

	err := conn.QueryRow(ctx, `
		SELECT COALESCE(a.wait_event_type || ':' || a.wait_event, ''),
		       COALESCE(pg_blocking_pids(a.pid), '{}'),
		       COALESCE((SELECT b.query FROM pg_stat_activity b
		                  WHERE b.pid = ANY(pg_blocking_pids(a.pid))
		                    AND b.query <> '' LIMIT 1), ''),
		       COALESCE(p.phase, ''),
		       CASE
		         WHEN p.blocks_total > 0
		           THEN round(100.0 * p.blocks_done / p.blocks_total, 2)
		         WHEN p.tuples_total > 0
		           THEN round(100.0 * p.tuples_done / p.tuples_total, 2)
		         ELSE NULL
		       END
		  FROM pg_stat_activity a
		  LEFT JOIN pg_stat_progress_create_index p ON p.pid = a.pid
		 WHERE a.pid = $1`, pid).
		Scan(&p.WaitEvent, &p.BlockedBy, &p.BlockerQuery, &p.Phase, &percent)
	if err != nil {
		return nil, err
	}
	if percent != nil {
		v := *percent
		p.Percent = &v
	}
	return &p, nil
}

// backendPID asks the connection for its own process id, so the observer knows
// which session to watch.
func backendPID(ctx context.Context, conn *pgx.Conn) (int32, error) {
	var pid int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// logTransitions writes an event for each way this observation differs from the
// one before it, and nothing at all when they agree.
//
// waitSince is carried across calls so that the end of a wait can report how
// long it lasted, which is the number a person actually wants: "blocked for
// eleven minutes" says something that "blocked" does not.
func (e *Executor) logTransitions(ctx context.Context, executionID int64, prev, now store.Progress, waitSince *time.Time) {
	record := func(ev store.Event) {
		if err := e.store.AppendEvent(ctx, executionID, ev); err != nil {
			e.log.Debug("recording an execution event failed", "error", err)
		}
	}

	switch {
	case len(now.BlockedBy) > 0 && len(prev.BlockedBy) == 0:
		*waitSince = time.Now()
		e.log.Warn("migration is waiting on another session",
			"blocked_by", now.BlockedBy, "wait", now.WaitEvent)
		record(store.Warn("wait.began",
			fmt.Sprintf("waiting on %s, held by %v", waitName(now.WaitEvent), now.BlockedBy)).
			With(map[string]any{
				"wait_event":    now.WaitEvent,
				"blocked_by":    now.BlockedBy,
				"blocker_query": now.BlockerQuery,
			}))

	case len(now.BlockedBy) == 0 && len(prev.BlockedBy) > 0:
		waited := time.Since(*waitSince).Round(time.Second)
		record(store.Info("wait.ended",
			fmt.Sprintf("no longer waiting, after %s", waited)).
			With(map[string]any{"waited_seconds": waited.Seconds()}))
	}

	// A phase change is the one progress signal worth recording unconditionally:
	// there are only a handful of them in an index build, and each marks real
	// work starting.
	if now.Phase != "" && now.Phase != prev.Phase {
		record(store.Info("progress.phase", "index build: "+now.Phase).
			With(map[string]any{"phase": now.Phase, "percent": now.Percent}))
	}

	// Percentages, by contrast, change on nearly every sample, so they are
	// recorded at coarse milestones instead.
	if crossed, ok := milestone(prev.Percent, now.Percent); ok {
		record(store.Info("progress.percent",
			fmt.Sprintf("index build %d%% complete", crossed)).
			With(map[string]any{"percent": now.Percent, "phase": now.Phase}))
	}
}

// milestone reports a twenty-percent boundary newly crossed between two
// samples, so a long build leaves five entries rather than several hundred.
func milestone(prev, now *float64) (int, bool) {
	if now == nil {
		return 0, false
	}
	was := 0.0
	if prev != nil {
		was = *prev
	}
	const step = 20.0
	crossed := int(*now/step) * int(step)
	if crossed == 0 || int(was/step)*int(step) >= crossed {
		return 0, false
	}
	return crossed, true
}

// waitName renders a wait event for a person. PostgreSQL's own names are
// precise but opaque; the common one is worth spelling out.
func waitName(event string) string {
	switch event {
	case "":
		return "an unnamed wait"
	case "Lock:relation":
		return "a lock on the table"
	default:
		return event
	}
}
