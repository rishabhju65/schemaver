package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAConcurrentBuildWaitsRatherThanAbandoning reproduces a failure that was
// reachable with nothing more exotic than an open SELECT.
//
// CREATE INDEX CONCURRENTLY waits on concurrent transactions internally, and
// those waits go through the lock manager — so the transactional lock timeout
// aborted them. Ten seconds meant a build was abandoned because somebody's
// report had been running for ten seconds, and abandoning one leaves an index
// PostgreSQL will never use.
//
// Unlike an ALTER, this statement takes ShareUpdateExclusiveLock and blocks no
// reader or writer while it waits, so waiting costs nothing.
func TestAConcurrentBuildWaitsRatherThanAbandoning(t *testing.T) {
	url := os.Getenv("SCHEMAVER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_TEST_DATABASE_URL to run the lock tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last. A deferred close runs as this function
	// returns, which is before any cleanup callback — so the drop below would
	// execute against a closed connection and silently leave the fixture
	// behind, where the next test to introspect this database finds it.
	t.Cleanup(func() { admin.Close(context.Background()) })

	// Its own schema, because other packages introspect this database whole and
	// a table in public is a table they will see.
	if _, err := admin.Exec(ctx, `
		DROP SCHEMA IF EXISTS lock_probe CASCADE;
		CREATE SCHEMA lock_probe;
		CREATE TABLE lock_probe.t (id bigint, v text);
		INSERT INTO lock_probe.t SELECT g, 'x' FROM generate_series(1, 20000) g`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS lock_probe CASCADE`)
	})

	// Somebody's transaction, open across the build. An ordinary read: it takes
	// ACCESS SHARE, which does not conflict with the build's lock at all.
	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM lock_probe.t`).Scan(new(int64)); err != nil {
		t.Fatalf("hold: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(6 * time.Second)
		_ = tx.Rollback(context.Background())
		holder.Close(context.Background())
		close(released)
	}()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect builder: %v", err)
	}
	defer conn.Close(context.Background())

	e := New(nil, nil)
	// Generous enough to outlast the holder, which is the whole point.
	e.ConcurrentLockTimeout = time.Minute
	err = e.runStandalone(ctx, conn, store.Step{
		Ordinal: 1, ChangeID: "create_index:lock_probe.t.t_v_idx",
		SQL: `CREATE INDEX CONCURRENTLY t_v_idx ON lock_probe.t (v)`,
	})
	<-released
	if err != nil {
		t.Fatalf("the build was abandoned while an ordinary read was open: %v", err)
	}

	var valid bool
	if err := admin.QueryRow(ctx, `
		SELECT i.indisvalid FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 't_v_idx'`).Scan(&valid); err != nil {
		t.Fatalf("the build left no index at all: %v", err)
	}
	if !valid {
		t.Error("the build left an index PostgreSQL will never use")
	}
}

// TestTheTransactionalTimeoutStillFailsFast is the other half. The two values
// exist because the arguments point opposite ways, and loosening one must not
// loosen the other: an ALTER waiting for a lock queues behind readers and then
// blocks every later query on that table.
func TestTheTransactionalTimeoutStillFailsFast(t *testing.T) {
	e := New(nil, nil)
	if e.LockTimeout >= e.ConcurrentLockTimeout {
		t.Errorf("the transactional timeout (%s) is not shorter than the "+
			"concurrent one (%s); the whole reason for two values is that an "+
			"ALTER must give up quickly and a concurrent build must not",
			e.LockTimeout, e.ConcurrentLockTimeout)
	}
	if e.LockTimeout > 30*time.Second {
		t.Errorf("a transactional lock timeout of %s is long enough to take a "+
			"table down while it waits", e.LockTimeout)
	}
	if strings.Contains(e.ConcurrentLockTimeout.String(), "0s") &&
		e.ConcurrentLockTimeout == 0 {
		t.Error("an unlimited concurrent timeout waits forever behind a stuck " +
			"transaction, and an execution that never returns says nothing")
	}
}
