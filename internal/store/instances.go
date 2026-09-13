package store

import (
	"context"
	"fmt"
	"time"
)

// InstanceRow is one registered server, for the instances list.
type InstanceRow struct {
	ID        int64
	Name      string
	Host      string
	Port      int
	TLSMode   string
	Username  string
	Engine    string
	Databases int
	Managed   int
	LastSeen  *time.Time
}

// Instances lists registered servers.
func (s *Scope) Instances(ctx context.Context) ([]InstanceRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT i.id, i.name, i.host, i.port, i.tls_mode, c.username, i.engine,
		       (SELECT count(*) FROM schemaver.database d
		         WHERE d.instance_id = i.id AND d.archived_at IS NULL),
		       (SELECT count(*) FROM schemaver.database d
		         WHERE d.instance_id = i.id AND d.archived_at IS NULL AND d.managed),
		       (SELECT max(d.last_checked_at) FROM schemaver.database d
		         WHERE d.instance_id = i.id)
		  FROM schemaver.instance i
		  JOIN schemaver.credential c ON c.id = i.credential_id
		 WHERE i.archived_at IS NULL AND i.project_id = ANY($1)
		 ORDER BY i.name`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()

	var out []InstanceRow
	for rows.Next() {
		var r InstanceRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Host, &r.Port, &r.TLSMode,
			&r.Username, &r.Engine, &r.Databases, &r.Managed, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ManagedDatabase is one database on an instance, with how it is configured.
// Maintenance reports the database every PostgreSQL installation creates for
// itself.
//
// `initdb` makes one called postgres as a place to connect before any real
// database exists, and it is also schemaver's own default for the database to
// open first. It is almost never something to version-control, and it turns up
// on servers whose console does not admit to it — a Neon project shows one
// database in its dashboard and has two on the endpoint, because Neon creates
// this one and does not surface it. Somebody who has just registered a server
// then finds a database they are certain they did not create.
//
// A hint, not a restriction: it can still be managed by anyone who means to.
func (d ManagedDatabase) Maintenance() bool { return d.Name == "postgres" }

type ManagedDatabase struct {
	ID   int64
	Name string
	// Managed is the soft toggle: whether schemaver watches this database.
	// Retired is the end-state, and outranks it — a retired database is never
	// managed, and the settings form must not offer to turn it back on.
	Managed       bool
	Retired       *time.Time
	RetiredReason string
	EnvironmentID *int64
	PeerID        *int64
	SizeBytes     int64
	LastError     string
}

// InstanceDetail returns one instance and every database discovered on it.
func (s *Scope) InstanceDetail(ctx context.Context, id int64) (*InstanceRow, []ManagedDatabase, error) {
	var inst InstanceRow
	err := s.store.pool.QueryRow(ctx, `
		SELECT i.id, i.name, i.host, i.port, i.tls_mode, c.username, i.engine
		  FROM schemaver.instance i
		  JOIN schemaver.credential c ON c.id = i.credential_id
		 WHERE i.id = $1 AND i.archived_at IS NULL AND i.project_id = ANY($2)`, id, s.projects).
		Scan(&inst.ID, &inst.Name, &inst.Host, &inst.Port, &inst.TLSMode,
			&inst.Username, &inst.Engine)
	if err != nil {
		return nil, nil, fmt.Errorf("load instance %d: %w", id, err)
	}

	rows, err := s.store.pool.Query(ctx, `
		SELECT id, name, managed, retired_at, COALESCE(retired_reason, ''),
		       environment_id, expected_peer_id,
		       COALESCE(size_bytes, 0), COALESCE(last_error, '')
		  FROM schemaver.database
		 WHERE instance_id = $1 AND archived_at IS NULL
		   AND instance_id IN (SELECT id FROM schemaver.instance WHERE project_id = ANY($2))
		 ORDER BY name`, id, s.projects)
	if err != nil {
		return nil, nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var dbs []ManagedDatabase
	for rows.Next() {
		var d ManagedDatabase
		if err := rows.Scan(&d.ID, &d.Name, &d.Managed, &d.Retired, &d.RetiredReason,
			&d.EnvironmentID,
			&d.PeerID, &d.SizeBytes, &d.LastError); err != nil {
			return nil, nil, fmt.Errorf("scan database: %w", err)
		}
		dbs = append(dbs, d)
	}
	return &inst, dbs, rows.Err()
}

// EnvironmentRow is a deployment tier.
type EnvironmentRow struct {
	ID   int64
	Name string
	Rank int
}

// Environments lists tiers in promotion order.
func (s *Scope) Environments(ctx context.Context) ([]EnvironmentRow, error) {
	rows, err := s.store.pool.Query(ctx,
		`SELECT id, name, rank FROM schemaver.environment
		  WHERE project_id = ANY($1) ORDER BY rank`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	var out []EnvironmentRow
	for rows.Next() {
		var e EnvironmentRow
		if err := rows.Scan(&e.ID, &e.Name, &e.Rank); err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DatabaseSettings is one database's configuration as submitted by a form.
type DatabaseSettings struct {
	ID            int64
	Managed       bool
	EnvironmentID *int64
	PeerID        *int64
}

// ApplyDatabaseSettings saves the management choices for an instance.
//
// Applied in one transaction so a partly-saved form cannot leave half the
// databases observed and half not.
func (s *Scope) ApplyDatabaseSettings(ctx context.Context, instanceID int64, settings []DatabaseSettings) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, set := range settings {
		// A database compared against itself would report drift against its own
		// schema forever; the database rejects it, but catching it here gives a
		// better message than a constraint violation.
		if set.PeerID != nil && *set.PeerID == set.ID {
			return fmt.Errorf("a database cannot be compared against itself")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.database
			   SET managed = $2, environment_id = $3, expected_peer_id = $4
			 WHERE id = $1 AND instance_id = $5
			   AND instance_id IN (SELECT id FROM schemaver.instance WHERE project_id = ANY($6))`,
			set.ID, set.Managed, set.EnvironmentID, set.PeerID, instanceID,
			s.projects); err != nil {
			return fmt.Errorf("save settings for database %d: %w", set.ID, err)
		}
	}
	return tx.Commit(ctx)
}
