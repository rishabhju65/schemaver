package introspect

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Database describes one database on an instance, as reported by the instance
// itself.
type Database struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Encoding    string `json:"encoding"`
	SizeBytes   int64  `json:"size_bytes"`
	Connectable bool   `json:"connectable"`
}

// Databases lists the databases on the instance q is connected to.
//
// Templates and databases that disallow connections are excluded: they cannot be
// managed and listing them only invites the user to try. Size is reported as
// zero where the role lacks CONNECT, because pg_database_size errors rather than
// returning null in that case.
func Databases(ctx context.Context, q Querier) ([]Database, error) {
	rows, err := q.Query(ctx, `
		SELECT d.datname,
		       pg_get_userbyid(d.datdba),
		       pg_encoding_to_char(d.encoding),
		       has_database_privilege(d.datname, 'CONNECT'),
		       CASE WHEN has_database_privilege(d.datname, 'CONNECT')
		            THEN pg_database_size(d.datname) ELSE 0 END
		FROM pg_database d
		WHERE NOT d.datistemplate AND d.datallowconn
		ORDER BY d.datname`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		var d Database
		if err := rows.Scan(&d.Name, &d.Owner, &d.Encoding, &d.Connectable, &d.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan database: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DatabaseSchema pairs a database with the schema read from it.
//
// Err is set when that particular database could not be read. It is a field
// rather than a returned error because one unreadable database must not discard
// the results for every other database on the instance — a restricted role
// commonly sees some and not others, and reporting that honestly is more useful
// than failing the whole snapshot.
type DatabaseSchema struct {
	Database
	Schema  *schema.Schema `json:"schema,omitempty"`
	Version schema.Version `json:"version,omitempty"`
	ReadMS  int64          `json:"read_ms"`
	Err     string         `json:"error,omitempty"`
}

// Instance connects to every database on the instance addressed by url and reads
// each one's schema.
//
// url may name any database on the instance; it is used only to enumerate, and
// each database is then connected to individually. Connections are opened and
// closed per database rather than pooled, so that introspecting a production
// instance never holds connections it does not need.
func Instance(ctx context.Context, url string) ([]DatabaseSchema, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to instance: %w", err)
	}
	list, err := Databases(ctx, admin)
	admin.Close(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]DatabaseSchema, 0, len(list))
	for _, db := range list {
		out = append(out, readOne(ctx, cfg, db))
	}
	return out, nil
}

func readOne(ctx context.Context, base *pgx.ConnConfig, db Database) DatabaseSchema {
	res := DatabaseSchema{Database: db}
	if !db.Connectable {
		res.Err = "no CONNECT privilege for this database"
		return res
	}

	start := time.Now()
	defer func() { res.ReadMS = time.Since(start).Milliseconds() }()

	cfg := base.Copy()
	cfg.Database = db.Name

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		res.Err = fmt.Sprintf("connect: %v", err)
		return res
	}
	defer conn.Close(context.Background())

	s, err := Schema(ctx, conn)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	v, err := schema.Fingerprint(s)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.Schema, res.Version = s, v
	return res
}
