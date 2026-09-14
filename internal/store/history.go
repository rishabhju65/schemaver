package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rishabhju65/schemaver/internal/history"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// TimelineEntry is one observed change to one database.
//
// Attribution is deliberately absent for now: with no executor, every change we
// see arrived from outside schemaver. Once migrations can be applied, this gains
// the migration that caused it — and an entry with no migration is precisely
// what drift means.
type TimelineEntry struct {
	SnapshotID  int64
	DatabaseID  int64
	Database    string
	Instance    string
	Environment string
	ObservedAt  time.Time
	From        schema.Version
	To          schema.Version
	ReadMS      *int32
	Error       string
}

// Failed reports whether this entry records a failed read rather than a change.
func (e TimelineEntry) Failed() bool { return e.Error != "" }

// Initial reports whether this is the first schema recorded for the database,
// which has no predecessor to compare against.
func (e TimelineEntry) Initial() bool { return e.From == "" && e.To != "" }

// TimelineFilter narrows a history query.
type TimelineFilter struct {
	// DatabaseID limits to one database; zero means every database.
	DatabaseID int64
	Limit      int
	Offset     int
}

// Timeline returns observed changes, newest first.
//
// The previous fingerprint is computed over successful reads only, so a failed
// observation does not break the chain between the changes either side of it.
func (s *Scope) Timeline(ctx context.Context, f TimelineFilter) ([]TimelineEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	var dbFilter *int64
	if f.DatabaseID != 0 {
		dbFilter = &f.DatabaseID
	}

	rows, err := s.store.pool.Query(ctx, `
		WITH observed AS (
		    SELECT s.id,
		           LAG(s.fingerprint) OVER (
		               PARTITION BY s.database_id ORDER BY s.observed_at) AS previous
		      FROM schemaver.snapshot s
		     WHERE s.fingerprint IS NOT NULL
		)
		SELECT s.id, s.database_id, d.name, i.name,
		       COALESCE(e.name, ''), s.observed_at,
		       COALESCE(o.previous, ''), COALESCE(s.fingerprint, ''),
		       s.read_ms, COALESCE(s.error, '')
		  FROM schemaver.snapshot s
		  JOIN schemaver.database d ON d.id = s.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		  LEFT JOIN observed o ON o.id = s.id
		 WHERE ($1::bigint IS NULL OR s.database_id = $1)
		   AND i.project_id = ANY($4)
		 ORDER BY s.observed_at DESC, s.id DESC
		 LIMIT $2 OFFSET $3`, dbFilter, f.Limit, f.Offset, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read timeline: %w", err)
	}
	defer rows.Close()

	var out []TimelineEntry
	for rows.Next() {
		var e TimelineEntry
		var from, to string
		if err := rows.Scan(&e.SnapshotID, &e.DatabaseID, &e.Database, &e.Instance,
			&e.Environment, &e.ObservedAt, &from, &to, &e.ReadMS, &e.Error); err != nil {
			return nil, fmt.Errorf("scan timeline entry: %w", err)
		}
		e.From, e.To = schema.Version(from), schema.Version(to)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Blob loads a stored schema by its fingerprint.
func (s *Scope) Blob(ctx context.Context, fingerprint schema.Version) (*schema.Schema, error) {
	if fingerprint == "" {
		return nil, nil
	}
	// Reachable only through something of this account's own that names the
	// schema. A fingerprint is unguessable, but scoping the lookup means the
	// guarantee does not rest on that.
	//
	// Three reasons, and only the first is an observation. Most schemas are
	// here because a database was read in that state. The other two are
	// computed: a merged schema and a branch head were never on any database,
	// so no snapshot will ever mention them, and on observations alone a
	// project could not read back the target its own migration declares or the
	// schema its own branch is at. Scoping holds throughout — the migration or
	// the branch has to belong to this project.
	var canonical []byte
	if err := s.store.pool.QueryRow(ctx, `
		SELECT b.canonical FROM schemaver.schema_blob b
		 WHERE b.fingerprint = $1
		   AND (EXISTS (
		       -- Observed on a database of ours.
		       SELECT 1 FROM schemaver.snapshot sn
		         JOIN schemaver.database d ON d.id = sn.database_id
		         JOIN schemaver.instance i ON i.id = d.instance_id
		        WHERE sn.fingerprint = b.fingerprint AND i.project_id = ANY($2))
		    OR EXISTS (
		       -- An end or an ancestor of one of our migrations.
		       SELECT 1 FROM schemaver.migration m
		         JOIN schemaver.change_request r ON r.id = m.change_request_id
		        WHERE b.fingerprint IN (m.from_fingerprint, m.to_fingerprint, m.merge_base)
		          AND r.project_id = ANY($2))
		    OR EXISTS (
		       -- Somewhere one of our branches has been.
		       SELECT 1 FROM schemaver.branch br
		         LEFT JOIN schemaver.branch_commit bc ON bc.branch_id = br.id
		        WHERE b.fingerprint IN (br.base_fingerprint, br.head_fingerprint,
		                                bc.from_fingerprint, bc.to_fingerprint)
		          AND br.project_id = ANY($2)))`,
		string(fingerprint), s.projects).Scan(&canonical); err != nil {
		return nil, fmt.Errorf("load schema %s: %w", fingerprint.Short(), err)
	}
	var out schema.Schema
	if err := json.Unmarshal(canonical, &out); err != nil {
		return nil, fmt.Errorf("decode schema %s: %w", fingerprint.Short(), err)
	}
	return &out, nil
}

// DatabaseRow is one row of the fleet view.
type DatabaseRow struct {
	ID          int64
	Name        string
	Instance    string
	Environment string
	Managed     bool
	// Retired is when somebody stood this database down, or nil while it is
	// still live. RetiredReason is why, in their words.
	Retired       *time.Time
	RetiredReason string
	// Writable reports that changes can still be aimed here. Kept as a field
	// rather than derived in the template, so the one definition of writable
	// lives in SQL beside the others.
	Writable      bool
	Fingerprint   schema.Version
	LastCheckedAt *time.Time
	LastReadAt    *time.Time
	LastError     string
	OpenDrifts    int
	Changes       int
}

// Fleet lists every registered database with its current state.
//
// Staleness is left for the caller to judge from LastReadAt rather than being
// reduced to a boolean here: how old is too old depends on the polling interval,
// and a view that hides the age would present stale data as current.
func (s *Scope) Fleet(ctx context.Context) ([]DatabaseRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT d.id, d.name, i.name, COALESCE(e.name, ''), d.managed,
		       d.retired_at, COALESCE(d.retired_reason, ''),
		       `+writableDatabase+`,
		       COALESCE(d.current_fingerprint, ''),
		       d.last_checked_at, d.last_read_at, COALESCE(d.last_error, ''),
		       (SELECT count(*) FROM schemaver.drift f
		         WHERE f.database_id = d.id AND f.status = 'open'),
		       (SELECT count(*) FROM schemaver.snapshot s
		         WHERE s.database_id = d.id AND s.fingerprint IS NOT NULL)
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		 WHERE d.archived_at IS NULL AND i.project_id = ANY($1)
		 ORDER BY i.name, COALESCE(e.rank, 2147483647), d.name`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read fleet: %w", err)
	}
	defer rows.Close()

	var out []DatabaseRow
	for rows.Next() {
		var r DatabaseRow
		var fingerprint string
		if err := rows.Scan(&r.ID, &r.Name, &r.Instance, &r.Environment, &r.Managed,
			&r.Retired, &r.RetiredReason, &r.Writable,
			&fingerprint, &r.LastCheckedAt, &r.LastReadAt, &r.LastError,
			&r.OpenDrifts, &r.Changes); err != nil {
			return nil, fmt.Errorf("scan fleet row: %w", err)
		}
		r.Fingerprint = schema.Version(fingerprint)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DriftRow is one open or resolved divergence.
type DriftRow struct {
	ID int64
	// DatabaseID and PeerID carry the identities behind the names, so a reader
	// looking at a divergence can act on it rather than having to find both
	// databases again by hand on another page.
	DatabaseID  int64
	PeerID      *int64
	Database    string
	Instance    string
	Environment string
	Observed    schema.Version
	Expected    schema.Version
	Source      string
	Peer        string
	Status      string
	FirstSeen   time.Time
	LastSeen    time.Time

	// Changed is which objects differ, and Summary counts them. Two
	// fingerprints say that a database is not where it should be and nothing
	// about what is wrong, which leaves the only way to find out being to open
	// a change request — backwards, since what changed is how somebody decides
	// whether to open one.
	//
	// Worked out from the two stored schemas rather than recorded when the
	// divergence opened: a drift row is one row with a moving last_seen, and
	// what differs today is not what differed when it was first noticed.
	Changed []history.ObjectChange
	Summary history.Summary

	// Unexplained is set where the difference could not be worked out — one of
	// the two schemas is not readable. Said plainly rather than shown as "no
	// changes", which would be the same display as agreement.
	Unexplained string
}

// Drifts lists divergences, open ones first.
func (s *Scope) Drifts(ctx context.Context, includeResolved bool) ([]DriftRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT f.id, d.id, f.peer_database_id, d.name, i.name, COALESCE(e.name, ''),
		       f.observed_fingerprint, f.expected_fingerprint,
		       f.expected_source, COALESCE(p.name, ''),
		       f.status, f.first_seen, f.last_seen
		  FROM schemaver.drift f
		  JOIN schemaver.database d ON d.id = f.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		  LEFT JOIN schemaver.database p ON p.id = f.peer_database_id
		 WHERE ($1::boolean OR f.status = 'open') AND i.project_id = ANY($2)
		 ORDER BY (f.status = 'open') DESC, f.last_seen DESC`, includeResolved, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read drifts: %w", err)
	}
	defer rows.Close()

	var out []DriftRow
	for rows.Next() {
		var r DriftRow
		var observed, expected string
		if err := rows.Scan(&r.ID, &r.DatabaseID, &r.PeerID,
			&r.Database, &r.Instance, &r.Environment,
			&observed, &expected, &r.Source, &r.Peer, &r.Status,
			&r.FirstSeen, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("scan drift row: %w", err)
		}
		r.Observed, r.Expected = schema.Version(observed), schema.Version(expected)
		s.explain(ctx, &r)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DatabaseSummary is what a database is, as distinct from what has happened to
// it.
type DatabaseSummary struct {
	ID          int64
	Name        string
	Instance    string
	InstanceID  int64
	Environment string
	// Follows names the database a change passes through before this one, or is
	// empty where nothing precedes it.
	Follows  string
	Managed  bool
	Retired  bool
	Observed schema.Version
}

// Database reads one database's configuration.
//
// Its own query rather than a filter over Fleet: this answers "what is this
// database" for a page about one, and the fleet answers "what is out there" for
// a page about all of them. Sharing a shape would tie two pages together that
// will drift apart.
func (s *Scope) Database(ctx context.Context, id int64) (*DatabaseSummary, error) {
	var d DatabaseSummary
	var fingerprint string
	err := s.store.pool.QueryRow(ctx, `
		SELECT d.id, d.name, i.name, i.id, COALESCE(e.name, ''),
		       COALESCE(p.name, ''), d.managed, d.retired_at IS NOT NULL,
		       COALESCE(d.current_fingerprint, '')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		  LEFT JOIN schemaver.database p ON p.id = d.expected_peer_id
		 WHERE d.id = $1 AND i.project_id = ANY($2)`, id, s.projects).
		Scan(&d.ID, &d.Name, &d.Instance, &d.InstanceID, &d.Environment,
			&d.Follows, &d.Managed, &d.Retired, &fingerprint)
	if err != nil {
		return nil, fmt.Errorf("load database %d: %w", id, err)
	}
	d.Observed = schema.Version(fingerprint)
	return &d, nil
}

// explain works out what actually differs about a divergence.
//
// Failures are recorded on the row rather than returned. A drift page that will
// not load because one schema of one database could not be read is worse than
// one that shows every divergence and admits it cannot describe one of them —
// and the fingerprints, which are the part that matters for deciding something
// is wrong, are already in hand either way.
func (s *Scope) explain(ctx context.Context, r *DriftRow) {
	if r.Observed == "" || r.Expected == "" {
		r.Unexplained = "one side has not been read"
		return
	}
	expected, err := s.Blob(ctx, r.Expected)
	if err != nil || expected == nil {
		r.Unexplained = "the schema it should match is no longer readable"
		return
	}
	observed, err := s.Blob(ctx, r.Observed)
	if err != nil || observed == nil {
		r.Unexplained = "the schema it is at is no longer readable"
		return
	}
	r.Changed = history.ObjectsChanged(expected, observed)
	r.Summary = history.Count(r.Changed)
	if len(r.Changed) == 0 {
		// Different fingerprints with no object differing means the difference
		// is inside an object rather than in which objects exist — a column, a
		// constraint, an index. Saying "nothing changed" would contradict the
		// fingerprints on the same row.
		r.Unexplained = "the difference is inside an object rather than in which objects exist"
	}
}
