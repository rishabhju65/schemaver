package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestSettingsCannotReachAnotherTenant covers a hole that scoping the row does
// not close: scoping the values written into it.
//
// ApplyDatabaseSettings filtered which database could be changed, by project,
// and said nothing about what it could be changed *to*. A crafted form could
// therefore point one tenant's database at another tenant's — putting that
// database's name on the attacker's fleet, drift page and database page, and
// its fingerprint into their drift records once the comparison ran. The same
// applied to the environment.
//
// Scoping a write means scoping both ends of it, and the refusal must not
// distinguish "belongs to somebody else" from "does not exist", or it becomes a
// way to enumerate what other tenants have.
func TestSettingsCannotReachAnotherTenant(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the isolation test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	// Two tenants of this test's own, so no real deployment is touched.
	type tenant struct {
		org, project, instance, database, environment int64
	}
	make := func(label string) tenant {
		var tn tenant
		if err := pool.QueryRow(ctx,
			`INSERT INTO schemaver.organization (name) VALUES ($1) RETURNING id`,
			"iso-"+label).Scan(&tn.org); err != nil {
			t.Fatalf("create org %s: %v", label, err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.project (name, organization_id)
			VALUES ($1, $2) RETURNING id`, "iso-"+label, tn.org).Scan(&tn.project); err != nil {
			t.Fatalf("create project %s: %v", label, err)
		}
		var credential int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.credential (project_id, name, username, kind, secret_ref)
			VALUES ($1, $2, 'u', 'secret_ref', 'r') RETURNING id`,
			tn.project, "iso-"+label).Scan(&credential); err != nil {
			t.Fatalf("create credential %s: %v", label, err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.instance (project_id, name, host, credential_id)
			VALUES ($1, $2, 'iso.invalid', $3) RETURNING id`,
			tn.project, "iso-"+label, credential).Scan(&tn.instance); err != nil {
			t.Fatalf("create instance %s: %v", label, err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed)
			VALUES ($1, $2, true) RETURNING id`,
			tn.instance, "db-"+label).Scan(&tn.database); err != nil {
			t.Fatalf("create database %s: %v", label, err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.environment (name, rank, project_id)
			VALUES ('production', 20, $1) RETURNING id`, tn.project).Scan(&tn.environment); err != nil {
			t.Fatalf("create environment %s: %v", label, err)
		}
		return tn
	}

	attacker := make("attacker")
	victim := make("victim")
	t.Cleanup(func() {
		bg := context.Background()
		for _, tn := range []tenant{attacker, victim} {
			pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, tn.instance)
			pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, tn.instance)
			pool.Exec(bg, `DELETE FROM schemaver.organization WHERE id = $1`, tn.org)
		}
	})

	scope := st.ForProject(attacker.project)

	// Following a database belonging to somebody else.
	if err := scope.ApplyDatabaseSettings(ctx, attacker.instance,
		[]store.DatabaseSettings{{ID: attacker.database, Managed: true, PeerID: &victim.database}}); err == nil {
		t.Error("one tenant pointed their database at another tenant's; that " +
			"database's name then renders on their pages and its fingerprint " +
			"enters their drift records")
	} else if !strings.Contains(err.Error(), "no such database") {
		t.Errorf("the refusal distinguishes another tenant's database from a "+
			"missing one, which enumerates what others have: %v", err)
	}

	// Labelling with somebody else's environment.
	if err := scope.ApplyDatabaseSettings(ctx, attacker.instance,
		[]store.DatabaseSettings{{ID: attacker.database, Managed: true, EnvironmentID: &victim.environment}}); err == nil {
		t.Error("one tenant labelled their database with another tenant's environment")
	} else if !strings.Contains(err.Error(), "no such environment") {
		t.Errorf("the environment refusal is not the same as for a missing one: %v", err)
	}

	// Nothing landed.
	var peer, env *int64
	if err := pool.QueryRow(ctx, `
		SELECT expected_peer_id, environment_id FROM schemaver.database WHERE id = $1`,
		attacker.database).Scan(&peer, &env); err != nil {
		t.Fatalf("read the database back: %v", err)
	}
	if peer != nil {
		t.Errorf("a cross-tenant peer was written anyway: %d", *peer)
	}
	if env != nil {
		t.Errorf("a cross-tenant environment was written anyway: %d", *env)
	}

	// And the tenant's own values still work, so the check is not simply
	// refusing everything.
	if err := scope.ApplyDatabaseSettings(ctx, attacker.instance,
		[]store.DatabaseSettings{{ID: attacker.database, Managed: true,
			EnvironmentID: &attacker.environment}}); err != nil {
		t.Errorf("a tenant's own environment was refused: %v", err)
	}
}
