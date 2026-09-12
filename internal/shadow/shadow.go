// Package shadow runs schema work in throwaway databases.
//
// It serves two purposes that turn out to be the same mechanism:
//
//   - Reading a declared schema (product §11). Rather than parsing DDL, apply it
//     to an empty database and introspect the result. Declared and live schemas
//     then normalize through identical code and cannot disagree because of a
//     parser bug.
//   - Proving a migration's outcome (D-009). Build a database at the migration's
//     `from` fingerprint, apply the migration, and require the result to
//     fingerprint as `to`. That is a proof of correctness, not a test.
//
// Shadow databases are created on schemaver's own Postgres instance, never on a
// managed target. Nothing here ever touches a customer database.
package shadow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// namePrefix marks every database this package creates, so abandoned ones can be
// recognised and swept up.
const namePrefix = "schemaver_shadow_"

// Pool creates throwaway databases on one instance.
//
// The instance must be one we administer — in practice schemaver's own metadata
// server, which we ship and whose version we control. DROP DATABASE ... WITH
// (FORCE) requires PostgreSQL 13 or later.
type Pool struct {
	adminDSN string
}

// NewPool returns a Pool that creates databases on the instance addressed by
// adminDSN. The connection must have CREATEDB.
func NewPool(adminDSN string) (*Pool, error) {
	if _, err := pgx.ParseConfig(adminDSN); err != nil {
		return nil, fmt.Errorf("parse shadow admin url: %w", err)
	}
	return &Pool{adminDSN: adminDSN}, nil
}

// DB is one throwaway database. Always pair Create with Close.
type DB struct {
	Name string
	dsn  string
	pool *Pool
}

// newName builds a unique database name carrying its creation time, so that a
// sweep can identify abandoned databases without a registry to consult.
func newName() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate shadow database name: %w", err)
	}
	return fmt.Sprintf("%s%d_%s", namePrefix, time.Now().UnixMilli(), hex.EncodeToString(b[:])), nil
}

// Create makes a new empty database.
func (p *Pool) Create(ctx context.Context) (*DB, error) {
	name, err := newName()
	if err != nil {
		return nil, err
	}
	admin, err := pgx.Connect(ctx, p.adminDSN)
	if err != nil {
		return nil, fmt.Errorf("connect to shadow host: %w", err)
	}
	defer admin.Close(context.Background())

	// CREATE DATABASE cannot run inside a transaction, and the name cannot be a
	// bind parameter — it is generated here and matches [a-z0-9_] by
	// construction, never derived from user input.
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return nil, fmt.Errorf("create shadow database: %w", err)
	}
	dsn, err := withDatabase(p.adminDSN, name)
	if err != nil {
		return nil, err
	}
	return &DB{Name: name, dsn: dsn, pool: p}, nil
}

// Close drops the database.
//
// It takes its own context rather than reusing the caller's: cleanup must still
// run when the work was cancelled or timed out, which is exactly when the
// caller's context is already dead.
func (db *DB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, db.pool.adminDSN)
	if err != nil {
		return fmt.Errorf("connect to drop shadow database %s: %w", db.Name, err)
	}
	defer admin.Close(context.Background())

	// FORCE terminates any connection still open against it; without it a
	// lingering connection would leak the database indefinitely.
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+db.Name+`" WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop shadow database %s: %w", db.Name, err)
	}
	return nil
}

// Apply executes DDL against the database.
//
// The statements run through the simple query protocol, so the whole batch is
// one implicit transaction: either all of it applies or none does. That is the
// behaviour wanted for a declared schema, and it means this cannot be used for
// statements that refuse to run in a transaction, such as a concurrent index
// build. Those are the executor's problem, not the shadow's.
func (db *DB) Apply(ctx context.Context, ddl string) error {
	if strings.TrimSpace(ddl) == "" {
		return nil
	}
	conn, err := pgx.Connect(ctx, db.dsn)
	if err != nil {
		return fmt.Errorf("connect to shadow database: %w", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("apply ddl to shadow database: %w", err)
	}
	return nil
}

// Schema introspects the database.
func (db *DB) Schema(ctx context.Context) (*schema.Schema, error) {
	conn, err := pgx.Connect(ctx, db.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to shadow database: %w", err)
	}
	defer conn.Close(context.Background())
	return introspect.Schema(ctx, conn)
}

// Load reads a declared schema by applying it to a throwaway database and
// introspecting the result — product §11.
//
// DDL that fails to apply is a schema error, and the engine's own message is the
// most useful thing we can report, so it is passed through rather than
// reinterpreted.
func (p *Pool) Load(ctx context.Context, ddl string) (*schema.Schema, schema.Version, error) {
	db, err := p.Create(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = db.Close() }()

	if err := db.Apply(ctx, ddl); err != nil {
		return nil, "", err
	}
	s, err := db.Schema(ctx)
	if err != nil {
		return nil, "", err
	}
	v, err := schema.Fingerprint(s)
	if err != nil {
		return nil, "", err
	}
	return s, v, nil
}

// MismatchError reports a migration that did not produce the schema it claimed.
type MismatchError struct {
	Want, Got schema.Version
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf(
		"migration did not produce the schema it declares: expected %s, produced %s",
		e.Want.Short(), e.Got.Short())
}

// Verify proves a migration's outcome, per D-009.
//
// It builds a database from baseDDL, applies the migration, and requires the
// result to fingerprint as want. A mismatch means the migration is wrong — not
// risky, wrong — and the returned MismatchError is a hard block.
//
// This proves the schema outcome only. The shadow holds no data, so it cannot
// find rows that violate a new constraint, cannot estimate duration, and says
// nothing about lock behaviour under load. It must never be reported as evidence
// that a migration is safe (D-009's scope cut).
func (p *Pool) Verify(ctx context.Context, baseDDL, migrationDDL string, want schema.Version) (schema.Version, error) {
	db, err := p.Create(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()

	if err := db.Apply(ctx, baseDDL); err != nil {
		return "", fmt.Errorf("build base schema: %w", err)
	}
	if err := db.Apply(ctx, migrationDDL); err != nil {
		return "", fmt.Errorf("apply migration: %w", err)
	}
	s, err := db.Schema(ctx)
	if err != nil {
		return "", err
	}
	got, err := schema.Fingerprint(s)
	if err != nil {
		return "", err
	}
	if got != want {
		return got, &MismatchError{Want: want, Got: got}
	}
	return got, nil
}

// Sweep drops shadow databases older than age.
//
// A crash between Create and Close leaves a database behind with nothing
// tracking it, so the creation time is encoded in the name and recovered here.
// Without this, an unlucky restart loop fills the instance with abandoned
// databases.
func (p *Pool) Sweep(ctx context.Context, age time.Duration) (int, error) {
	admin, err := pgx.Connect(ctx, p.adminDSN)
	if err != nil {
		return 0, fmt.Errorf("connect to shadow host: %w", err)
	}
	defer admin.Close(context.Background())

	rows, err := admin.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1`, namePrefix+"%")
	if err != nil {
		return 0, fmt.Errorf("list shadow databases: %w", err)
	}
	var stale []string
	cutoff := time.Now().Add(-age).UnixMilli()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan shadow database name: %w", err)
		}
		if created, ok := createdAt(name); ok && created < cutoff {
			stale = append(stale, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dropped := 0
	for _, name := range stale {
		// Deliberately without FORCE, unlike Close. Close drops a database this
		// process created and still owns, where a lingering connection is its
		// own to cut. Sweep drops databases it merely recognises by name, and
		// on a shared server those belong to other people: another deployment,
		// a test run, a proof the worker is in the middle of.
		//
		// Refusing to drop an in-use database is the whole safety property
		// here. An abandoned one has no connections — the process that held
		// them is gone, which is why it was abandoned — so it drops cleanly,
		// and crash recovery still works. A live one refuses and is skipped.
		//
		// With FORCE this swept the server rather than its own leavings: age
		// zero terminated every open shadow connection on the host, which is
		// exactly what it did to the diff and render suites whenever they
		// happened to overlap with it.
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`"`); err != nil {
			continue
		}
		dropped++
	}
	return dropped, nil
}

// createdAt recovers the creation timestamp encoded in a shadow database name.
func createdAt(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return 0, false
	}
	millis, _, ok := strings.Cut(rest, "_")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// withDatabase rewrites a connection URL to point at a different database.
func withDatabase(dsn, database string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse connection url: %w", err)
	}
	u.Path = "/" + database
	return u.String(), nil
}
