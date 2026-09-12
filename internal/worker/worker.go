// Package worker runs the observation loop: it keeps schemaver's picture of every
// managed database current.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/drift"
	"github.com/rishabhju65/schemaver/internal/executor"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/shadow"
	"github.com/rishabhju65/schemaver/internal/store"
)

// Config tunes the loop.
type Config struct {
	// ID identifies this worker in job leases.
	ID string
	// Interval is how often each managed database is checked.
	Interval time.Duration
	// FullReadAfter forces a complete introspection even when the cheap probe
	// reports no change. This is the backstop for the probe's blind spots: any
	// field the probe fails to cover becomes visible within one of these periods
	// rather than never.
	FullReadAfter time.Duration
	// Lease is how long a claimed job is held before another worker may reclaim
	// it. It must exceed the slowest expected introspection.
	Lease time.Duration
	// Timeout bounds a single observation.
	Timeout time.Duration
	// ExecutionTimeout bounds a single migration. Generous, because a concurrent
	// index build on a large table legitimately takes hours.
	ExecutionTimeout time.Duration
	// Concurrency is how many observation jobs this worker runs at once.
	Concurrency int
	// Shadow creates throwaway databases to prove migrations against. Nil
	// disables proving, which is reported on each migration rather than passed
	// over: a check that quietly does not run is worse than no check, because
	// everything downstream still speaks as though it did.
	Shadow *shadow.Pool

	// ExecutionConcurrency is a separate pool for migrations. Separate because a
	// migration can hold a worker for an hour: sharing one pool would let a
	// handful of long migrations stop drift detection entirely for that hour.
	ExecutionConcurrency int
	// Budget bounds how much work one instance may carry at once, derived from
	// its observed connection headroom rather than fixed.
	Budget store.Budget
}

func (c *Config) setDefaults() {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if c.FullReadAfter <= 0 {
		c.FullReadAfter = 24 * time.Hour
	}
	if c.Lease <= 0 {
		c.Lease = 10 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.ExecutionTimeout <= 0 {
		c.ExecutionTimeout = 4 * time.Hour
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.ExecutionConcurrency <= 0 {
		c.ExecutionConcurrency = 4
	}
	if c.Budget.Ceiling == 0 {
		c.Budget = store.DefaultBudget()
	}
	if c.ID == "" {
		c.ID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
}

// Worker claims jobs and executes them.
//
// Many instances are worked simultaneously; that is the normal case. Work spreads
// across Concurrency goroutines, admitted while the weight in flight against an
// instance stays inside that instance's budget — which is derived from its
// observed connection headroom rather than fixed, so a server permitting a
// thousand connections is not held to the same limit as one permitting a hundred.
//
// The budget is enforced by the claim query rather than here, so it holds across
// worker processes. Scaling to a large fleet is therefore a matter of running
// more processes, not of tuning this one.
type Worker struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger
	exec  *executor.Executor
}

func New(s *store.Store, cfg Config, log *slog.Logger) *Worker {
	cfg.setDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: s, cfg: cfg, log: log, exec: executor.New(s, log)}
}

// Run schedules due work and drains the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.schedule(ctx)
	}()

	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.drainLoop(ctx, fmt.Sprintf("%s/obs-%d", w.cfg.ID, n), store.ObservationKinds)
		}(i)
	}
	for i := 0; i < w.cfg.ExecutionConcurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.drainLoop(ctx, fmt.Sprintf("%s/exec-%d", w.cfg.ID, n), store.ExecutionKinds)
		}(i)
	}

	wg.Wait()
	return ctx.Err()
}

// schedule enqueues due work on a fixed interval.
func (w *Worker) schedule(ctx context.Context) {
	tick := time.NewTicker(w.cfg.Interval)
	defer tick.Stop()

	if _, err := w.store.EnqueueDue(ctx, w.cfg.Interval); err != nil && ctx.Err() == nil {
		w.log.Error("initial scheduling failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := w.store.EnqueueDue(ctx, w.cfg.Interval)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Error("scheduling failed", "error", err)
				}
				continue
			}
			if n > 0 {
				w.log.Debug("scheduled work", "jobs", n)
			}
		}
	}
}

// drainLoop is one concurrent claimant. Polls are jittered so that idle workers
// do not all hit the claim query on the same tick.
func (w *Worker) drainLoop(ctx context.Context, id string, kinds []string) {
	for {
		if ctx.Err() != nil {
			return
		}
		worked := w.drainOnce(ctx, id, kinds)
		if worked {
			continue
		}
		delay := time.Second + time.Duration(rand.Int63n(int64(2*time.Second)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// drainOnce claims and runs a single job, reporting whether it found one.
func (w *Worker) drainOnce(ctx context.Context, id string, kinds []string) bool {
	job, err := w.store.ClaimJob(ctx, id, w.cfg.Lease, w.cfg.Budget, kinds)
	if err == store.ErrNoJob {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("claiming job failed", "error", err)
		}
		return false
	}

	// A migration may run far longer than an observation, and must not be cut
	// short by the observation timeout.
	timeout := w.cfg.Timeout
	if job.Kind == store.KindExecute {
		timeout = w.cfg.ExecutionTimeout
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout)

	// Hold the lease for as long as the work takes. Without this a long
	// migration has its job reclaimed while still running: the advisory lock
	// stops a second worker doing damage, but the bookkeeping goes wrong and the
	// interface then lies about what is happening.
	stopHeartbeat := w.heartbeat(jobCtx, job.ID, id)
	runErr := w.handle(jobCtx, job)
	stopHeartbeat()
	cancel()

	if runErr != nil {
		w.log.Warn("job failed", "job", job.ID, "kind", job.Kind,
			"instance", job.InstanceID, "attempts", job.Attempts, "error", runErr)
	}
	// Closed with the parent context: a job that timed out still has to be
	// released, and jobCtx is already expired.
	if err := w.store.FinishJob(ctx, job, runErr); err != nil && ctx.Err() == nil {
		w.log.Error("closing job failed", "job", job.ID, "error", err)
	}
	return true
}

// heartbeat renews a claimed job's lease until the returned function is called.
//
// A failure to renew means the lease already lapsed and somebody else holds the
// job, so the loop stops rather than fighting for it. The advisory lock is what
// prevents damage in that window; this only keeps the record honest.
func (w *Worker) heartbeat(ctx context.Context, jobID int64, workerID string) func() {
	done := make(chan struct{})
	interval := w.cfg.Lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				held, err := w.store.RenewLease(context.WithoutCancel(ctx), jobID, workerID, w.cfg.Lease)
				if err != nil {
					w.log.Warn("renewing lease failed", "job", jobID, "error", err)
					continue
				}
				if !held {
					w.log.Warn("lease lapsed and was taken; stopping renewal", "job", jobID)
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (w *Worker) handle(ctx context.Context, job *store.Job) error {
	switch job.Kind {
	case store.KindDiscover:
		return w.discover(ctx, job.TargetID)
	case store.KindObserve:
		return w.observe(ctx, job.TargetID)
	case store.KindExecute:
		return w.execute(ctx, job.TargetID)
	case store.KindProve:
		return w.prove(ctx, job.TargetID)
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// execute applies a migration and records where it ended up.
//
// The outcome is always recorded, including when it is one nobody wants. A
// migration that halted ambiguously must leave a visible NEEDS_ATTENTION and a
// reason, never an absence.
func (w *Worker) execute(ctx context.Context, migrationID int64) error {
	x, err := w.store.LoadExecution(ctx, migrationID)
	if err != nil {
		return err
	}
	w.log.Info("executing migration", "migration", migrationID,
		"database", x.DatabaseName, "statements", len(x.Steps),
		"from", x.From.Short(), "to", x.To.Short())

	// Visible before the first statement runs, so the interface shows work in
	// flight rather than a gap between queued and finished.
	if serr := w.store.SetRequestState(ctx, x.RequestID, "EXECUTING",
		fmt.Sprintf("applying %d statement(s) to %s", len(x.Steps), x.DatabaseName)); serr != nil {
		w.log.Warn("marking the request as executing failed", "error", serr)
	}

	outcome, err := w.exec.Execute(ctx, x)
	if err != nil {
		// We could not get far enough to have an outcome. Record that rather
		// than leaving the request looking as though it is still running.
		_ = w.store.SetRequestState(context.WithoutCancel(ctx), x.RequestID,
			executor.StateNeedsAttention,
			fmt.Sprintf("execution could not be carried out: %v", err))
		return err
	}

	if serr := w.store.SetRequestState(context.WithoutCancel(ctx), x.RequestID,
		outcome.State, outcome.Reason); serr != nil {
		w.log.Error("recording the outcome failed", "migration", migrationID, "error", serr)
	}

	switch outcome.State {
	case executor.StateCompleted:
		w.log.Info("migration applied", "migration", migrationID,
			"database", x.DatabaseName, "version", outcome.Final.Short())
	case executor.StateNeedsAttention:
		w.log.Error("MIGRATION HALTED — a human must decide",
			"migration", migrationID, "database", x.DatabaseName,
			"detail", outcome.Reason)
	default:
		w.log.Warn("migration did not apply", "migration", migrationID,
			"state", outcome.State, "detail", outcome.Reason)
	}

	// A retryable failure is the job's business; anything else is settled and
	// must not be attempted again automatically.
	if outcome.Retryable {
		return errors.New(outcome.Reason)
	}
	return nil
}

// discover reconciles the databases known on an instance against what it reports.
func (w *Worker) discover(ctx context.Context, instanceID int64) error {
	// "postgres" is the conventional bootstrap database; it is used only to
	// enumerate, never introspected through this connection.
	dsn, err := w.store.InstanceDSN(ctx, instanceID, "postgres")
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to instance %d: %w", instanceID, err)
	}
	defer conn.Close(context.Background())

	found, err := introspect.Databases(ctx, conn)
	if err != nil {
		return err
	}
	added, archived, err := w.store.SyncDatabases(ctx, instanceID, found)
	if err != nil {
		return err
	}
	if added > 0 || archived > 0 {
		w.log.Info("instance databases changed",
			"instance", instanceID, "added", added, "archived", archived)
	}
	return nil
}

// observe brings one database's recorded state up to date.
//
// The cheap probe runs first; a full read happens only when the probe reports a
// change or the full-read backstop is due. Failures are recorded rather than
// swallowed, and the last known schema is left in place so the interface can
// present it as stale instead of losing it.
func (w *Worker) observe(ctx context.Context, databaseID int64) error {
	target, err := w.store.LoadTarget(ctx, databaseID, w.cfg.FullReadAfter)
	if err != nil {
		return err
	}

	if err := w.read(ctx, target); err != nil {
		if recErr := w.store.RecordFailure(ctx, databaseID, err); recErr != nil {
			w.log.Error("recording failure failed", "database", databaseID, "error", recErr)
		}
		return err
	}

	// Drift is evaluated on every cycle, not only when the schema changed: an
	// expectation can move while the database stands still — a new declared
	// schema is imported, or the peer being compared against is migrated — and
	// that is drift arriving without any observation of this database changing.
	// The comparison is two stored fingerprints, so doing it every time is free.
	return w.evaluateDrift(ctx, databaseID, target.Name)
}

func (w *Worker) evaluateDrift(ctx context.Context, databaseID int64, name string) error {
	observed, err := w.store.CurrentFingerprint(ctx, databaseID)
	if err != nil {
		return err
	}
	expectation, err := w.store.Expectation(ctx, databaseID)
	if err != nil {
		return err
	}

	result := drift.Evaluate(observed, expectation)
	opened, resolved, err := w.store.RecordDrift(ctx, databaseID, result)
	if err != nil {
		return err
	}
	if opened {
		w.log.Warn("drift detected", "database", name, "source", result.Source,
			"detail", result.Reason)
	}
	if resolved > 0 {
		w.log.Info("drift resolved", "database", name, "closed", resolved)
	}
	return nil
}

func (w *Worker) read(ctx context.Context, t *store.Target) error {
	start := time.Now()
	conn, err := pgx.Connect(ctx, t.DSN)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())

	// Sampled on a connection that is open anyway, so the budget stays current
	// without a schedule of its own.
	if cap, err := introspect.SampleCapacity(ctx, conn); err == nil {
		if err := w.store.RecordCapacity(ctx, t.InstanceID, cap.Max, cap.Reserved, cap.Used); err != nil {
			w.log.Warn("recording connection capacity failed", "error", err)
		}
	}

	digest, err := introspect.Probe(ctx, conn)
	if err != nil {
		return err
	}
	if digest == t.ProbeDigest && t.ProbeDigest != "" && !t.FullReadDue {
		return w.store.MarkUnchanged(ctx, t.DatabaseID, digest)
	}

	sch, err := introspect.Schema(ctx, conn)
	if err != nil {
		return err
	}
	fingerprint, err := schema.Fingerprint(sch)
	if err != nil {
		return err
	}

	changed, err := w.store.RecordSchema(ctx, t.DatabaseID, digest, sch,
		fingerprint, time.Since(start).Milliseconds())
	if err != nil {
		return err
	}
	if changed {
		w.log.Info("schema changed",
			"database", t.Name, "version", fingerprint.Short())
	}
	return nil
}
