package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// twoServers stands up a project with a database on each of two servers, which
// is how staging and production are normally arranged — staging on its own
// host precisely so its load never touches production's.
func twoServers(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (projectID, prodInstance, prod, stg int64) {
	t.Helper()
	var credentialID, userID int64
	if err := pool.QueryRow(ctx, `
		SELECT m.project_id, m.user_id, i.credential_id
		  FROM schemaver.project_member m
		  JOIN schemaver.instance i ON i.project_id = m.project_id
		 WHERE m.role = 'admin' ORDER BY m.project_id LIMIT 1`).
		Scan(&projectID, &userID, &credentialID); err != nil {
		t.Skipf("no project with an administrator: %v", err)
	}

	var ids []int64
	for _, s := range []struct{ name, host string }{
		{"peer-prod-" + t.Name(), strings.ToLower(t.Name()) + "-prod.invalid"},
		{"peer-stg-" + t.Name(), strings.ToLower(t.Name()) + "-stg.invalid"},
	} {
		var instanceID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.instance (project_id, name, host, credential_id)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			projectID, s.name, s.host, credentialID).Scan(&instanceID); err != nil {
			t.Fatalf("create server %s: %v", s.name, err)
		}
		ids = append(ids, instanceID)
		t.Cleanup(func() {
			bg := context.Background()
			pool.Exec(bg, `UPDATE schemaver.database SET expected_peer_id = NULL
			                WHERE instance_id = $1`, instanceID)
			pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instanceID)
			pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
		})
	}

	// Deliberately the same database name on both hosts, which is the ordinary
	// case and the one a list of bare names cannot express.
	for i, instanceID := range ids {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed)
			VALUES ($1, 'appdb', true) RETURNING id`, instanceID).Scan(&id); err != nil {
			t.Fatalf("create database: %v", err)
		}
		if i == 0 {
			prod, prodInstance = id, instanceID
		} else {
			stg = id
		}
	}
	_ = userID
	return projectID, prodInstance, prod, stg
}

// TestADatabaseCanFollowOneOnAnotherServer is the arrangement the promotion
// chain exists for, and the one the selector could not express.
//
// Nothing underneath ever required them to be neighbours: the peer is validated
// against the project, the cycle check walks the chain wherever it goes, and the
// direction check compares environments rather than addresses. Only the list of
// options was drawn from one server.
func TestADatabaseCanFollowOneOnAnotherServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, prodInstance, prod, stg := twoServers(ctx, t, pool)
	scope := st.ForProject(projectID)

	choices, err := scope.PeerChoices(ctx, prod)
	if err != nil {
		t.Fatalf("PeerChoices: %v", err)
	}
	var offered bool
	for _, c := range choices {
		if c.ID == stg {
			offered = true
			if c.SameServer {
				t.Error("a database on another server is marked as a sibling")
			}
			if !strings.Contains(c.Label(), "peer-stg") {
				t.Errorf("the label does not say which server: %q", c.Label())
			}
		}
		if c.ID == prod {
			t.Error("a database is offered as something to follow itself")
		}
	}
	if !offered {
		t.Fatal("a database on another server is not offered; the promotion " +
			"chain cannot be built for the arrangement it exists to serve")
	}

	// A choice that would never compare anything says so, rather than being
	// hidden — an absent option turns "why is my database not listed" into a
	// mystery, and the answer is worth more than the absence.
	var unmanaged int64
	pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed)
		VALUES ((SELECT instance_id FROM schemaver.database WHERE id = $1), 'unwatched', false)
		RETURNING id`, stg).Scan(&unmanaged)
	after, err := scope.PeerChoices(ctx, prod)
	if err != nil {
		t.Fatalf("PeerChoices: %v", err)
	}
	var sawUnmanaged bool
	for _, c := range after {
		if c.ID != unmanaged {
			continue
		}
		sawUnmanaged = true
		if c.Managed {
			t.Error("an unmanaged database is offered as though it were watched")
		}
		if !strings.Contains(c.Label(), "not managed") {
			t.Errorf("the label does not say it would compare nothing: %q", c.Label())
		}
	}
	if !sawUnmanaged {
		t.Error("an unmanaged database was hidden rather than explained")
	}

	// And setting it actually works, which the store always allowed.
	if err := scope.ApplyDatabaseSettings(ctx, prodInstance, []store.DatabaseSettings{
		{ID: prod, Managed: true, PeerID: &stg},
	}); err != nil {
		t.Fatalf("following across servers was refused: %v", err)
	}
	var got *int64
	pool.QueryRow(ctx, `SELECT expected_peer_id FROM schemaver.database WHERE id = $1`,
		prod).Scan(&got)
	if got == nil || *got != stg {
		t.Error("the peer was not recorded")
	}
}

// TestFollowingStillRefusesACycleAcrossServers. The chain deadlocks the gate in
// silence — every member waits for one below it — and crossing a server is no
// reason for that check to stop applying.
func TestFollowingStillRefusesACycleAcrossServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, prodInstance, prod, stg := twoServers(ctx, t, pool)
	scope := st.ForProject(projectID)

	if err := scope.ApplyDatabaseSettings(ctx, prodInstance, []store.DatabaseSettings{
		{ID: prod, Managed: true, PeerID: &stg},
	}); err != nil {
		t.Fatalf("first link: %v", err)
	}
	var stgInstance int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		stg).Scan(&stgInstance)
	if err := scope.ApplyDatabaseSettings(ctx, stgInstance, []store.DatabaseSettings{
		{ID: stg, Managed: true, PeerID: &prod},
	}); err == nil {
		t.Error("a two-database cycle across servers was accepted; every member " +
			"waits for one below it, so none may ever go first")
	}
}
