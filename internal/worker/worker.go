// Package worker runs the observation loop: it keeps schemaver's picture of every
// managed database current.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/drift"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
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
	// Timeout bounds a single job.
	Timeout time.Duration
	// Concurrency is how many jobs this worker runs at once, across all
	// instances.
	Concurrency int
	// PerInstance caps how many jobs may run against any single instance at
	// once. Instances are worked in parallel; databases within one instance are
	// not, beyond this limit.
	PerInstance int
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
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.PerInstance <= 0 {
		c.PerInstance = 2
	}
	if c.ID == "" {
		c.ID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
}

// Worker claims jobs and executes them.
//
// Many instances are observed simultaneously; that is the normal case. Work is
// spread across Concurrency goroutines, with no more than PerInstance jobs in
// flight against any one instance. The per-instance limit is enforced by the
// claim query rather than here, so it holds across worker processes too, and
// instances at capacity are skipped rather than waited on — a slow instance
// never stalls progress on the others.
type Worker struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger
}

func New(s *store.Store, cfg Config, log *slog.Logger) *Worker {
	cfg.setDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: s, cfg: cfg, log: log}
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
			w.drainLoop(ctx, fmt.Sprintf("%s/%d", w.cfg.ID, n))
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
func (w *Worker) drainLoop(ctx context.Context, id string) {
	for {
		if ctx.Err() != nil {
			return
		}
		worked := w.drainOnce(ctx, id)
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
func (w *Worker) drainOnce(ctx context.Context, id string) bool {
	job, err := w.store.ClaimJob(ctx, id, w.cfg.Lease, w.cfg.PerInstance)
	if err == store.ErrNoJob {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("claiming job failed", "error", err)
		}
		return false
	}

	jobCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	runErr := w.handle(jobCtx, job)
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

func (w *Worker) handle(ctx context.Context, job *store.Job) error {
	switch job.Kind {
	case "discover":
		return w.discover(ctx, job.TargetID)
	case "observe":
		return w.observe(ctx, job.TargetID)
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
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
