package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/introspect"
)

// Candidate is a database on a server that this project does not yet manage.
type Candidate struct {
	Name     string
	Owner    string
	Encoding string
	Size     int64
	// Connectable is false where the credential cannot open it, which is worth
	// showing rather than hiding: "it is there and you cannot reach it" is a
	// different problem from "it is not there".
	Connectable bool
}

// AddDatabase brings one database under management, creating the server record
// if this is the first database on it.
//
// The database is the thing somebody asks for. A server is what it happens to
// live on — worth recording, because databases sharing a host share its
// connection limit and its credentials, and worth nothing on its own.
//
// Managed by construction. A row that exists because a person typed its name is
// a row they want; the alternative was rows arriving unasked and sitting inert
// until somebody noticed them.
func (s *Scope) AddDatabase(
	ctx context.Context, actorID int64,
	server, host string, port int, tlsMode, username, password, database string,
	environmentID int64,
) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return 0, errors.New("which database?")
	}

	// An existing server with the same address is reused rather than
	// duplicated: two databases on one host really do share its connection
	// budget, and modelling them as two servers would let the product open
	// twice what the host allows.
	var instanceID int64
	err := s.store.pool.QueryRow(ctx, `
		SELECT id FROM schemaver.instance
		 WHERE project_id = ANY($1) AND host = $2 AND port = $3`,
		s.projects, host, port).Scan(&instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		name := strings.TrimSpace(server)
		if name == "" {
			name = fmt.Sprintf("%s:%d", host, port)
		}
		instanceID, err = s.RegisterInstance(ctx, name, host, port, tlsMode, username, password)
		if err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, fmt.Errorf("look for an existing server: %w", err)
	}

	return s.adopt(ctx, actorID, instanceID, environmentID, database)
}

// adopt creates the database row and asks for it to be read.
//
// environmentID may be zero, meaning the most guarded environment the project
// has. That default came from discovery (D-026) and has to survive discovery no
// longer creating anything: the label decides whether a promotion link points
// the right way, and an unlabelled database makes that check permissive because
// there are no ranks to compare. Of the two ways to be wrong only one is
// dangerous — calling staging production costs ceremony somebody removes, while
// calling production staging lets a change reach it without passing through
// anything.
//
// Where somebody added one database by hand they are asked outright, because
// they are standing there and know the answer. Zero is for the bulk path, where
// they are not being asked about each one.
func (s *Scope) adopt(ctx context.Context, actorID, instanceID, environmentID int64, name string) (int64, error) {
	var id int64
	err := s.store.pool.QueryRow(ctx, `
		INSERT INTO schemaver.database
		    (instance_id, name, managed, last_seen, environment_id)
		VALUES ($1, $2, true, now(), COALESCE(NULLIF($3, 0), (
		    SELECT e.id FROM schemaver.environment e
		      JOIN schemaver.instance i ON i.id = $1
		     WHERE e.project_id = i.project_id
		     ORDER BY e.rank DESC LIMIT 1)))
		ON CONFLICT (instance_id, name) DO UPDATE
		   SET managed = true, archived_at = NULL, last_seen = now(),
		       environment_id = COALESCE(EXCLUDED.environment_id,
		                                 schemaver.database.environment_id)
		RETURNING id`, instanceID, name, environmentID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add database %q: %w", name, err)
	}

	// Read it now rather than at the next cycle. Somebody who has just added a
	// database is looking at the page, and an empty one teaches them the
	// product does not work.
	if err := s.store.ObserveNow(ctx, id); err != nil {
		return 0, err
	}
	s.record(ctx, Info("database.added", "added "+name).By(actorID).OnDatabase(id))
	return id, nil
}

// AdoptDatabases brings several databases on a server under management at once.
func (s *Scope) AdoptDatabases(ctx context.Context, actorID, instanceID, environmentID int64, names []string) (int, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	var owned bool
	if err := s.store.pool.QueryRow(ctx, `
		SELECT true FROM schemaver.instance
		 WHERE id = $1 AND project_id = ANY($2)`, instanceID, s.projects).
		Scan(&owned); err != nil {
		return 0, errors.New("no such server")
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if _, err := s.adopt(ctx, actorID, instanceID, environmentID, name); err != nil {
			return 0, err
		}
	}
	return len(names), nil
}

// Discoverable lists databases on a server that this project does not manage.
//
// Read-only and creates nothing. That is the whole point of the change it
// belongs to: looking at what is on a host used to commit you to a row for
// every one of them, so the only way to find out was to be given the answer
// whether you wanted it or not.
//
// Connects directly rather than going through the job queue, because somebody
// is waiting for the list. It is one short-lived connection against a server
// the caller already has credentials for, not the sustained work the per-server
// budget exists to ration.
func (s *Scope) Discoverable(ctx context.Context, instanceID int64) ([]Candidate, error) {
	var owned bool
	if err := s.store.pool.QueryRow(ctx, `
		SELECT true FROM schemaver.instance
		 WHERE id = $1 AND project_id = ANY($2)`, instanceID, s.projects).
		Scan(&owned); err != nil {
		return nil, errors.New("no such server")
	}

	known := map[string]bool{}
	rows, err := s.store.pool.Query(ctx,
		`SELECT name FROM schemaver.database WHERE instance_id = $1`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read what is already here: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		known[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	dsn, err := s.store.InstanceDSN(ctx, instanceID, "postgres")
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to the server: %w", err)
	}
	defer conn.Close(context.Background())

	found, err := introspect.Databases(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}

	var out []Candidate
	for _, db := range found {
		if known[db.Name] {
			continue
		}
		out = append(out, Candidate{
			Name: db.Name, Owner: db.Owner, Encoding: db.Encoding,
			Size: db.SizeBytes, Connectable: db.Connectable,
		})
	}
	return out, nil
}
