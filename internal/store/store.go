// Package store is the only code that reads and writes schemaver's own metadata
// database.
//
// Everything here except the audit trail is derived state: it can be rebuilt by
// re-reading the repository and re-introspecting every managed database, per
// D-004. That is why writes are free to be destructive and why nothing here is
// treated as a system of record.
package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/secret"
)

// Store holds the metadata pool and the key used to open stored credentials.
type Store struct {
	pool *pgxpool.Pool
	box  *secret.Box
}

// New builds a Store over an existing pool.
func New(pool *pgxpool.Pool, box *secret.Box) *Store {
	return &Store{pool: pool, box: box}
}

// Pool exposes the underlying pool for callers that need raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ---------------------------------------------------------------- connections

// endpoint is everything needed to dial one database.
type endpoint struct {
	host, tlsMode, username, password, database string
	port                                        int
}

// dsn renders the endpoint as a connection URL.
func (e endpoint) dsn() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(e.username, e.password),
		Host:   net.JoinHostPort(e.host, strconv.Itoa(e.port)),
		Path:   "/" + e.database,
	}
	q := url.Values{}
	q.Set("sslmode", e.tlsMode)
	// Never let a slow or unreachable target hold a worker indefinitely.
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u.String()
}

// credentials decrypts an instance's stored secret.
func (s *Store) credentials(ctx context.Context, kind, ref string, ciphertext []byte) (string, error) {
	switch kind {
	case "inline":
		if s.box == nil {
			return "", errors.New("no encryption key configured; cannot open stored credentials")
		}
		return s.box.Open(ciphertext)
	case "secret_ref":
		return "", fmt.Errorf("credential %q is an external secret reference, which is not yet supported", ref)
	default:
		return "", fmt.Errorf("unknown credential kind %q", kind)
	}
}

// InstanceDSN returns a connection string for an instance, pointed at the
// database named by fallback (used only to enumerate).
func (s *Store) InstanceDSN(ctx context.Context, instanceID int64, fallback string) (string, error) {
	var e endpoint
	var kind, ref string
	var ciphertext []byte

	err := s.pool.QueryRow(ctx, `
		SELECT i.host, i.port, i.tls_mode, c.username, c.kind,
		       COALESCE(c.secret_ref, ''), COALESCE(c.secret_ciphertext, '\x'::bytea)
		FROM schemaver.instance i
		JOIN schemaver.credential c ON c.id = i.credential_id
		WHERE i.id = $1 AND i.archived_at IS NULL`, instanceID).
		Scan(&e.host, &e.port, &e.tlsMode, &e.username, &kind, &ref, &ciphertext)
	if err != nil {
		return "", fmt.Errorf("load instance %d: %w", instanceID, err)
	}
	if e.password, err = s.credentials(ctx, kind, ref, ciphertext); err != nil {
		return "", err
	}
	e.database = fallback
	return e.dsn(), nil
}

// Target is a managed database a worker is about to observe.
type Target struct {
	DatabaseID int64
	// InstanceID is carried so capacity can be recorded against the server while
	// a connection to one of its databases is already open.
	InstanceID  int64
	Name        string
	DSN         string
	ProbeDigest string
	// FullReadDue reports that the cheap probe must be skipped and a full read
	// performed regardless — the backstop that keeps a probe blind spot from
	// hiding a change indefinitely.
	FullReadDue bool
}

// LoadTarget assembles everything needed to observe one database.
func (s *Store) LoadTarget(ctx context.Context, databaseID int64, fullReadAfter time.Duration) (*Target, error) {
	var e endpoint
	var kind, ref string
	var ciphertext []byte
	var probe *string
	var lastRead *time.Time
	t := &Target{DatabaseID: databaseID}

	err := s.pool.QueryRow(ctx, `
		SELECT d.name, i.id, i.host, i.port, i.tls_mode, c.username, c.kind,
		       COALESCE(c.secret_ref, ''), COALESCE(c.secret_ciphertext, '\x'::bytea),
		       d.probe_digest, d.last_read_at
		FROM schemaver.database d
		JOIN schemaver.instance i ON i.id = d.instance_id
		JOIN schemaver.credential c ON c.id = i.credential_id
		WHERE d.id = $1 AND d.archived_at IS NULL AND i.archived_at IS NULL`, databaseID).
		Scan(&t.Name, &t.InstanceID, &e.host, &e.port, &e.tlsMode, &e.username,
			&kind, &ref, &ciphertext, &probe, &lastRead)
	if err != nil {
		return nil, fmt.Errorf("load database %d: %w", databaseID, err)
	}
	if e.password, err = s.credentials(ctx, kind, ref, ciphertext); err != nil {
		return nil, err
	}
	e.database = t.Name
	t.DSN = e.dsn()
	if probe != nil {
		t.ProbeDigest = *probe
	}
	t.FullReadDue = lastRead == nil || time.Since(*lastRead) > fullReadAfter
	return t, nil
}

// ------------------------------------------------------------------ discovery

// SyncDatabases reconciles the databases we know about on an instance with what
// the instance actually reports.
//
// Newly discovered databases are inserted unmanaged: discovery must never imply
// consent to manage. Databases that have disappeared are archived rather than
// deleted, so their snapshot history stays readable.
func (s *Store) SyncDatabases(ctx context.Context, instanceID int64, found []introspect.Database) (added, archived int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names := make([]string, len(found))
	for i, db := range found {
		names[i] = db.Name
		var inserted bool
		if err := tx.QueryRow(ctx, `
			INSERT INTO schemaver.database
			    (instance_id, name, owner, encoding, size_bytes, last_seen)
			VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (instance_id, name) DO UPDATE
			   SET owner = EXCLUDED.owner,
			       encoding = EXCLUDED.encoding,
			       size_bytes = EXCLUDED.size_bytes,
			       last_seen = now(),
			       archived_at = NULL
			RETURNING (xmax = 0)`,
			instanceID, db.Name, db.Owner, db.Encoding, db.SizeBytes).Scan(&inserted); err != nil {
			return 0, 0, fmt.Errorf("upsert database %q: %w", db.Name, err)
		}
		if inserted {
			added++
		}
	}

	tag, err := tx.Exec(ctx, `
		UPDATE schemaver.database SET archived_at = now()
		WHERE instance_id = $1 AND archived_at IS NULL AND NOT (name = ANY($2))`,
		instanceID, names)
	if err != nil {
		return 0, 0, fmt.Errorf("archive vanished databases: %w", err)
	}
	archived = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return added, archived, nil
}

// ---------------------------------------------------------------- observation

// MarkUnchanged records a successful probe that found no change. It writes no
// snapshot row — that is what keeps a stable database free to poll.
func (s *Store) MarkUnchanged(ctx context.Context, databaseID int64, digest string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE schemaver.database
		   SET probe_digest = $2, last_checked_at = now(), last_error = NULL
		 WHERE id = $1`, databaseID, digest)
	if err != nil {
		return fmt.Errorf("mark unchanged: %w", err)
	}
	return nil
}

// RecordSchema stores an observed schema and, when it differs from what we last
// saw, writes a snapshot marking the transition.
//
// Returns whether the schema changed.
func (s *Store) RecordSchema(
	ctx context.Context, databaseID int64, digest string,
	sch *schema.Schema, fingerprint schema.Version, readMS int64,
) (changed bool, err error) {
	canonical, err := schema.Canonical(sch)
	if err != nil {
		return false, fmt.Errorf("serialize schema: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current *string
	if err := tx.QueryRow(ctx,
		`SELECT current_fingerprint FROM schemaver.database WHERE id = $1 FOR UPDATE`,
		databaseID).Scan(&current); err != nil {
		return false, fmt.Errorf("lock database %d: %w", databaseID, err)
	}
	changed = current == nil || *current != string(fingerprint)

	// Content-addressed: an identical schema seen on a hundred databases is
	// stored once.
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, tableCount(sch), len(canonical)); err != nil {
		return false, fmt.Errorf("store schema blob: %w", err)
	}

	var snapshotID *int64
	if changed {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO schemaver.snapshot (database_id, fingerprint, read_ms)
			VALUES ($1, $2, $3) RETURNING id`,
			databaseID, string(fingerprint), readMS).Scan(&id); err != nil {
			return false, fmt.Errorf("write snapshot: %w", err)
		}
		snapshotID = &id
	}

	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.database
		   SET current_fingerprint = $2,
		       current_snapshot_id = COALESCE($3, current_snapshot_id),
		       probe_digest = $4,
		       last_checked_at = now(),
		       last_read_at = now(),
		       last_error = NULL
		 WHERE id = $1`,
		databaseID, string(fingerprint), snapshotID, digest); err != nil {
		return false, fmt.Errorf("update current state: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return changed, nil
}

// RecordFailure records that a database could not be read.
//
// The last known schema is deliberately left in place: it remains the best
// information available, and last_error together with the age of last_read_at is
// what tells the interface to present it as stale rather than current.
func (s *Store) RecordFailure(ctx context.Context, databaseID int64, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.snapshot (database_id, error) VALUES ($1, $2)`,
		databaseID, cause.Error()); err != nil {
		return fmt.Errorf("write failure snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.database SET last_error = $2, last_checked_at = now()
		 WHERE id = $1`, databaseID, cause.Error()); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	return tx.Commit(ctx)
}

func tableCount(s *schema.Schema) int {
	n := 0
	for _, ns := range s.Namespaces {
		n += len(ns.Tables)
	}
	return n
}

// ------------------------------------------------------------------ job queue

// Job is one unit of claimed work.
type Job struct {
	ID         int64
	Kind       string
	TargetKind string
	TargetID   int64
	InstanceID int64
	Attempts   int
}

// ErrNoJob signals an empty queue. It is an expected condition, not a failure.
var ErrNoJob = errors.New("no job available")

// Budget decides how much work an instance may carry at once.
//
// Derived from observed headroom rather than from configuration: a server
// permitting five hundred connections with four hundred and eighty in use has
// twenty spare, and a fixed cap would be either absurdly low on a large server
// or dangerous on a loaded one.
type Budget struct {
	// Floor is allowed whatever the headroom, so that a busy instance still
	// makes progress rather than deadlocking on its own load. Set at least to
	// the weight of one lock-heavy migration.
	Floor int
	// Ceiling bounds it however much headroom exists. Past a point more
	// concurrency makes heavy work slower rather than faster, because the
	// constraint becomes disk rather than connections.
	Ceiling int
	// Fraction is the share of observed headroom we are willing to consume. The
	// rest belongs to the application the database exists to serve.
	Fraction float64
}

// DefaultBudget is deliberately conservative. Consuming a sixth of what is spare
// leaves the application room to grow between our samples, and a ceiling of
// thirty-two is well past the point where more parallel index builds stop
// helping.
func DefaultBudget() Budget {
	return Budget{Floor: 4, Ceiling: 32, Fraction: 0.15}
}

// ClaimJob takes the next due job and holds it under a lease.
//
// SKIP LOCKED lets many workers claim concurrently without blocking each other.
// The lease, rather than a lock, is what makes a dead worker recoverable: its job
// becomes claimable again once the lease lapses, instead of sitting in 'running'
// forever.
//
// Work is admitted while the weight already in flight against an instance, plus
// this job's own weight, stays inside that instance's budget. Weight comes from
// what the work does — a metadata-only migration costs one, a table rewrite
// eight — because connections are not the binding constraint: two concurrent
// index builds compete for the same buffer cache however many slots are free.
//
// The whole calculation lives in the claim query rather than in worker memory,
// so it holds across every worker process rather than within one. That is what
// makes scaling out a matter of running more processes.
//
// Instances at capacity are skipped, never waited on, so a saturated instance
// never blocks work on any other.
// Kinds of work, kept as constants so the two pools cannot drift apart from the
// strings the scheduler enqueues.
const (
	KindObserve  = "observe"
	KindDiscover = "discover"
	KindExecute  = "execute"
	KindProve    = "prove"
)

// ObservationKinds is the cheap, short work.
var ObservationKinds = []string{KindObserve, KindDiscover, KindProve}

// ExecutionKinds is the long work. Claimed by a separate pool: a migration can
// hold a worker for an hour, and eight of them sharing one pool with observation
// would stop drift detection for that hour.
var ExecutionKinds = []string{KindExecute}

func (s *Store) ClaimJob(ctx context.Context, workerID string, lease time.Duration, budget Budget, kinds []string) (*Job, error) {
	if budget.Floor < 1 {
		budget.Floor = 1
	}
	if budget.Ceiling < budget.Floor {
		budget.Ceiling = budget.Floor
	}
	if budget.Fraction <= 0 {
		budget.Fraction = 0.1
	}
	var j Job
	var targetKind *string
	var targetID, instanceID *int64

	err := s.pool.QueryRow(ctx, `
		UPDATE schemaver.job
		   SET state = 'running', worker_id = $1,
		       lease_until = now() + $2::interval,
		       attempts = attempts + 1,
		       started_at = COALESCE(started_at, now())
		 WHERE id = (
		     SELECT j.id FROM schemaver.job j
		      LEFT JOIN schemaver.instance i ON i.id = j.instance_id
		      WHERE j.kind = ANY($6)
		        AND ((j.state = 'pending' AND j.run_after <= now())
		          OR (j.state = 'running' AND j.lease_until < now()))
		        -- A migration is claimable only while its starting point still
		        -- matches the database. Without this, a migration queued behind
		        -- another would be claimed, fail its precondition, back off and
		        -- retry — thrashing the queue instead of simply waiting. Ordering
		        -- becomes emergent rather than something the scheduler tracks.
		        AND (j.kind <> 'execute' OR EXISTS (
		              SELECT 1 FROM schemaver.migration m
		                JOIN schemaver.change_request r ON r.id = m.change_request_id
		                JOIN schemaver.database d ON d.id = r.database_id
		               WHERE m.id = j.target_id
		                 AND m.superseded_at IS NULL
		                 AND d.retired_at IS NULL
		                 AND d.current_fingerprint = m.from_fingerprint))
		        AND (j.instance_id IS NULL OR (
		              SELECT COALESCE(sum(r.weight), 0) FROM schemaver.job r
		               WHERE r.instance_id = j.instance_id
		                 AND r.state = 'running'
		                 AND r.lease_until > now()) + j.weight
		            <= GREATEST($3, LEAST($4, FLOOR((
		                   COALESCE(i.max_connections, 100)
		                 - COALESCE(i.reserved_connections, 3)
		                 -- $5 is cast explicitly. Left to inference, Postgres
		                 -- resolves "integer * unknown" to integer arithmetic
		                 -- and types the parameter as an integer, which silently
		                 -- truncates the fraction to zero: every instance then
		                 -- gets the floor as its budget, and work heavier than
		                 -- the floor is never claimable at all.
		                 - COALESCE(i.used_connections, 0)) * $5::float8)::int)))
		      ORDER BY j.run_after
		      -- Only the job row is locked: an outer join's nullable side cannot
		      -- be, and the instance row is read rather than claimed.
		      FOR UPDATE OF j SKIP LOCKED
		      LIMIT 1)
		RETURNING id, kind, target_kind, target_id, instance_id, attempts`,
		workerID, lease.String(), budget.Floor, budget.Ceiling, budget.Fraction, kinds).
		Scan(&j.ID, &j.Kind, &targetKind, &targetID, &instanceID, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	if targetKind != nil {
		j.TargetKind = *targetKind
	}
	if targetID != nil {
		j.TargetID = *targetID
	}
	if instanceID != nil {
		j.InstanceID = *instanceID
	}
	return &j, nil
}

// FinishJob closes out a claimed job. A failure is rescheduled with exponential
// backoff rather than retried immediately, so an unreachable host is not dialled
// every few seconds for a week.
func (s *Store) FinishJob(ctx context.Context, j *Job, cause error) error {
	if cause == nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE schemaver.job
			   SET state = 'succeeded', finished_at = now(), lease_until = NULL, error = NULL
			 WHERE id = $1`, j.ID)
		return err
	}
	backoff := time.Duration(1<<min(j.Attempts, 8)) * time.Minute
	_, err := s.pool.Exec(ctx, `
		UPDATE schemaver.job
		   SET state = 'pending', lease_until = NULL, worker_id = NULL,
		       error = $2, run_after = now() + $3::interval
		 WHERE id = $1`, j.ID, cause.Error(), backoff.String())
	return err
}

// EnqueueDue schedules observation jobs for managed databases that are due, and
// discovery jobs for instances.
//
// The idempotency key carries a time bucket, so re-running the scheduler within
// the same interval cannot enqueue the same work twice. Jitter is applied so
// that a hundred databases registered together do not all get polled in the same
// second forever after.
func (s *Store) EnqueueDue(ctx context.Context, every time.Duration) (int, error) {
	// Formatted here rather than cast in SQL: the cast makes Postgres infer the
	// parameter as text, which pgx then cannot encode an integer into.
	bucket := strconv.FormatInt(time.Now().UTC().Truncate(every).Unix(), 10)

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.job (kind, target_kind, target_id, instance_id, idempotency_key, run_after)
		SELECT 'observe', 'database', d.id, d.instance_id,
		       'observe:' || d.id || ':' || $1,
		       now() + (random() * $2::float * interval '1 second')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.managed AND d.archived_at IS NULL AND i.archived_at IS NULL
		ON CONFLICT (idempotency_key) DO NOTHING`,
		bucket, every.Seconds())
	if err != nil {
		return 0, fmt.Errorf("enqueue observations: %w", err)
	}
	n := int(tag.RowsAffected())

	tag, err = s.pool.Exec(ctx, `
		INSERT INTO schemaver.job (kind, target_kind, target_id, instance_id, idempotency_key, run_after)
		SELECT 'discover', 'instance', i.id, i.id,
		       'discover:' || i.id || ':' || $1,
		       now() + (random() * $2::float * interval '1 second')
		  FROM schemaver.instance i
		 WHERE i.archived_at IS NULL
		ON CONFLICT (idempotency_key) DO NOTHING`,
		bucket, every.Seconds())
	if err != nil {
		return 0, fmt.Errorf("enqueue discovery: %w", err)
	}
	return n + int(tag.RowsAffected()), nil
}

// RecordCapacity stores an instance's connection headroom, sampled while
// observing one of its databases.
//
// Sampled opportunistically rather than on a schedule of its own: the
// observation loop already connects to every managed database, so this costs one
// extra query on a connection that is open anyway.
func (s *Store) RecordCapacity(ctx context.Context, instanceID int64, max, reserved, used int) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.instance
		   SET max_connections = $2, reserved_connections = $3,
		       used_connections = $4, capacity_sampled_at = now()
		 WHERE id = $1`, instanceID, max, reserved, used); err != nil {
		return fmt.Errorf("record capacity: %w", err)
	}
	return nil
}

// RenewLease extends a claimed job's hold.
//
// Needed because a migration can run for far longer than any sensible lease. A
// forty-minute index build under a ten-minute lease would have its job reclaimed
// while still executing: the advisory lock stops the second worker doing damage,
// but the bookkeeping goes wrong and the interface then lies about what is
// happening.
//
// Reports false when the job is no longer ours, which means our lease already
// lapsed and somebody else has it — the caller should stop.
func (s *Store) RenewLease(ctx context.Context, jobID int64, workerID string, lease time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE schemaver.job
		   SET lease_until = now() + $3::interval
		 WHERE id = $1 AND worker_id = $2 AND state = 'running'`,
		jobID, workerID, lease.String())
	if err != nil {
		return false, fmt.Errorf("renew lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
