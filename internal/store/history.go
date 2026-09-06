package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	// Reachable only through this account's own observations. A fingerprint is
	// unguessable, but scoping the lookup means the guarantee does not rest on
	// that.
	var canonical []byte
	if err := s.store.pool.QueryRow(ctx, `
		SELECT b.canonical FROM schemaver.schema_blob b
		 WHERE b.fingerprint = $1
		   AND EXISTS (
		       SELECT 1 FROM schemaver.snapshot sn
		         JOIN schemaver.database d ON d.id = sn.database_id
		         JOIN schemaver.instance i ON i.id = d.instance_id
		        WHERE sn.fingerprint = b.fingerprint AND i.project_id = ANY($2)`,
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
	ID            int64
	Name          string
	Instance      string
	Environment   string
	Managed       bool
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
	ID          int64
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
}

// Drifts lists divergences, open ones first.
func (s *Scope) Drifts(ctx context.Context, includeResolved bool) ([]DriftRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT f.id, d.name, i.name, COALESCE(e.name, ''),
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
		if err := rows.Scan(&r.ID, &r.Database, &r.Instance, &r.Environment,
			&observed, &expected, &r.Source, &r.Peer, &r.Status,
			&r.FirstSeen, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("scan drift row: %w", err)
		}
		r.Observed, r.Expected = schema.Version(observed), schema.Version(expected)
		out = append(out, r)
	}
	return out, rows.Err()
}
