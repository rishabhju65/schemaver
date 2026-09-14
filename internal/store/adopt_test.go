package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestDiscoveryAdoptsNothingByItself is the change: enumerating a host used to
// create a row for every database on it, so connecting one server to manage one
// database dragged in the server's own `postgres`, another team's application,
// and whatever else shared the machine. They arrived unmanaged — harmless and
// pointless at once — and one of them needed a hardcoded apology in the fleet
// view to explain what it was doing there.
func TestDiscoveryAdoptsNothingByItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	_ = projectID
	var instanceID int64
	if err := pool.QueryRow(ctx,
		`SELECT instance_id FROM schemaver.database WHERE id = $1`, databaseID).
		Scan(&instanceID); err != nil {
		t.Fatalf("find the server: %v", err)
	}

	before := countDatabases(ctx, t, pool, instanceID)

	// The server reports the one we manage plus three nobody asked about.
	found := []introspect.Database{
		{Name: "branch_origin", Owner: "someone", Encoding: "UTF8", SizeBytes: 10},
		{Name: "postgres", Owner: "someone", Encoding: "UTF8", SizeBytes: 20},
		{Name: "someone_elses_app", Owner: "someone", Encoding: "UTF8", SizeBytes: 30},
		{Name: "analytics", Owner: "someone", Encoding: "UTF8", SizeBytes: 40},
	}
	if _, _, err := st.SyncDatabases(ctx, instanceID, found); err != nil {
		t.Fatalf("SyncDatabases: %v", err)
	}

	if after := countDatabases(ctx, t, pool, instanceID); after != before {
		t.Errorf("discovery created %d row(s) nobody asked for", after-before)
	}
	for _, name := range []string{"postgres", "someone_elses_app", "analytics"} {
		if databaseNamed(ctx, t, pool, instanceID, name) {
			t.Errorf("%q was adopted by discovery", name)
		}
	}
}

// TestDiscoveryStillArchivesWhatItManages: reconciling is the half worth
// keeping. A database we manage that is no longer on the host must be noticed.
func TestDiscoveryStillArchivesWhatItManages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	_, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	var instanceID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)

	// It reports nothing at all: what we manage has gone.
	if _, archived, err := st.SyncDatabases(ctx, instanceID, nil); err != nil {
		t.Fatalf("SyncDatabases: %v", err)
	} else if archived == 0 {
		t.Error("a managed database vanished from the server and nothing noticed")
	}

	var archivedAt *time.Time
	pool.QueryRow(ctx, `SELECT archived_at FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&archivedAt)
	if archivedAt == nil {
		t.Error("the database was not archived")
	}

	// And it comes back when the server reports it again, rather than being
	// stranded: the row is the record of something that existed.
	if _, _, err := st.SyncDatabases(ctx, instanceID, []introspect.Database{
		{Name: "branch_origin", Owner: "someone", Encoding: "UTF8", SizeBytes: 10},
	}); err != nil {
		t.Fatalf("SyncDatabases: %v", err)
	}
	pool.QueryRow(ctx, `SELECT archived_at FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&archivedAt)
	if archivedAt != nil {
		t.Error("the database reappeared on the server and stayed archived")
	}
}

// TestAdoptingIsDeliberateAndManaged: a row that exists because somebody named
// it is a row they want, so it arrives managed rather than inert.
func TestAdoptingIsDeliberateAndManaged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)
	var instanceID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)

	if _, err := scope.AdoptDatabases(ctx, userID, instanceID, 0, []string{"chosen_one"}); err != nil {
		t.Fatalf("AdoptDatabases: %v", err)
	}

	var managed bool
	if err := pool.QueryRow(ctx, `
		SELECT managed FROM schemaver.database
		 WHERE instance_id = $1 AND name = 'chosen_one'`, instanceID).
		Scan(&managed); err != nil {
		t.Fatalf("the adopted database is not there: %v", err)
	}
	if !managed {
		t.Error("a database somebody asked for by name arrived unmanaged")
	}
}

// TestAnotherTenantCannotAdopt.
func TestAnotherTenantCannotAdopt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	_, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	var instanceID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)

	var otherOrg, otherProject int64
	pool.QueryRow(ctx,
		`INSERT INTO schemaver.organization (name) VALUES ('adopt-iso') RETURNING id`).
		Scan(&otherOrg)
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.project WHERE organization_id = $1`, otherOrg)
		pool.Exec(bg, `DELETE FROM schemaver.organization WHERE id = $1`, otherOrg)
	})
	pool.QueryRow(ctx, `
		INSERT INTO schemaver.project (name, organization_id)
		VALUES ('adopt-iso', $1) RETURNING id`, otherOrg).Scan(&otherProject)

	if _, err := st.ForProject(otherProject).
		AdoptDatabases(ctx, userID, instanceID, 0, []string{"stolen"}); err == nil {
		t.Error("another tenant adopted a database onto a server it cannot see")
	}
	if databaseNamed(ctx, t, pool, instanceID, "stolen") {
		t.Error("the row was created despite the refusal")
	}
}

func countDatabases(ctx context.Context, t *testing.T, pool *pgxpool.Pool, instanceID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schemaver.database WHERE instance_id = $1`, instanceID).
		Scan(&n); err != nil {
		t.Fatalf("count databases: %v", err)
	}
	return n
}

func databaseNamed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, instanceID int64, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM schemaver.database
		                WHERE instance_id = $1 AND name = $2)`, instanceID, name).
		Scan(&exists); err != nil {
		t.Fatalf("look for %s: %v", name, err)
	}
	return exists
}
