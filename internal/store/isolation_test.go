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

	// Naming another tenant's database as the row to edit. The scoped UPDATE
	// meant the row itself never changed, which is what made this quiet: the
	// work done *around* the update ran on the unvalidated id anyway. A victim
	// paired database whose settings are saved with no peer gets its open drift
	// alarms resolved — by somebody in another organisation, with no error and
	// no trace on their own pages.
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed)
		VALUES ($1, 'db-victim-lower', true)`, victim.instance); err != nil {
		t.Fatalf("create the victim's lower database: %v", err)
	}
	var victimLower int64
	pool.QueryRow(ctx, `
		SELECT id FROM schemaver.database WHERE instance_id = $1 AND name = 'db-victim-lower'`,
		victim.instance).Scan(&victimLower)
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		victim.database, victimLower); err != nil {
		t.Fatalf("pair the victim: %v", err)
	}
	var observed, expected string
	if err := pool.QueryRow(ctx,
		`SELECT min(fingerprint), max(fingerprint) FROM schemaver.schema_blob`).
		Scan(&observed, &expected); err != nil || observed == expected {
		t.Skipf("need two schemas to raise an alarm: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.drift
		       (database_id, observed_fingerprint, expected_fingerprint, expected_source)
		VALUES ($1, $2, $3, 'declared')`, victim.database, observed, expected); err != nil {
		t.Fatalf("raise the victim's alarm: %v", err)
	}

	if err := scope.ApplyDatabaseSettings(ctx, attacker.instance,
		[]store.DatabaseSettings{{ID: victim.database, Managed: true}}); err == nil {
		t.Error("one tenant saved settings naming another tenant's database")
	} else if !strings.Contains(err.Error(), "no such database") {
		t.Errorf("the refusal names the foreign database rather than reading as "+
			"missing, which counts what others have: %v", err)
	}

	var alarm string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM schemaver.drift WHERE database_id = $1`,
		victim.database).Scan(&alarm); err != nil {
		t.Fatalf("read the victim's alarm back: %v", err)
	}
	if alarm != "open" {
		t.Errorf("the victim's drift alarm was %s by another organisation; for a "+
			"drift monitor, silently closing somebody else's alarm is the worst "+
			"of these", alarm)
	}

	var queued int
	pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job WHERE kind = 'observe' AND target_id = $1`,
		victim.database).Scan(&queued)
	if queued != 0 {
		t.Errorf("%d read(s) were queued against another tenant's database, which "+
			"makes schemaver connect to their server on an attacker's say-so", queued)
	}

	// And the tenant's own values still work, so the check is not simply
	// refusing everything.
	if err := scope.ApplyDatabaseSettings(ctx, attacker.instance,
		[]store.DatabaseSettings{{ID: attacker.database, Managed: true,
			EnvironmentID: &attacker.environment}}); err != nil {
		t.Errorf("a tenant's own environment was refused: %v", err)
	}
}

// TestClosingFreezesARequest covers what closing is for: an ending.
//
// COMPLETED says the statements ran. It does not say anybody is finished, and
// the rollback stays on offer afterwards because the hour after a change lands
// is when somebody decides it was wrong. Closing says that hour is over, and
// must then hold against every way of acting on a request — including the ones
// that had no state check at all before this, where a migration that had
// already run could still be approved.
func TestClosingFreezesARequest(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the closing test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, userID, target, source int64
	if err := pool.QueryRow(ctx, `
		SELECT i.project_id,
		       (SELECT m.user_id FROM schemaver.project_member m
		         WHERE m.project_id = i.project_id AND m.role = 'admin' LIMIT 1),
		       d.id, (SELECT id FROM schemaver.database WHERE name = 'shop_staging')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.name = 'shop_prod' AND d.current_fingerprint IS NOT NULL`).
		Scan(&projectID, &userID, &target, &source); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source, "closing probe", "")
	if err != nil {
		t.Skipf("nothing to migrate: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.change_request WHERE id = $1`, requestID)
	})
	migrationID, err := scope.GenerateMigration(ctx, userID, requestID)
	if err != nil {
		t.Skipf("nothing to generate: %v", err)
	}

	// A queued job, to check closing does not leave work that runs afterwards.
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.job (kind, target_kind, target_id, weight, idempotency_key)
		VALUES ('prove', 'migration', $1, 1, $2)
		ON CONFLICT (idempotency_key) DO UPDATE SET state = 'pending'`,
		migrationID, "closeprobe:"+time.Now().Format("150405.000")); err != nil {
		t.Fatalf("queue work: %v", err)
	}

	if err := scope.CloseRequest(ctx, userID, requestID, "not going ahead"); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}

	var state string
	var closedAt *time.Time
	var closedBy *int64
	if err := pool.QueryRow(ctx, `
		SELECT state, closed_at, closed_by FROM schemaver.change_request WHERE id = $1`,
		requestID).Scan(&state, &closedAt, &closedBy); err != nil {
		t.Fatalf("read the request back: %v", err)
	}
	if state != "CLOSED" || closedAt == nil || closedBy == nil {
		t.Fatalf("closing recorded state=%s at=%v by=%v", state, closedAt, closedBy)
	}

	var pending int
	pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job
		 WHERE target_kind = 'migration' AND target_id = $1
		   AND state IN ('pending', 'running')`, migrationID).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d job(s) still queued against a closed request; watching one "+
			"execute after it was declared finished is the worst reading of the "+
			"word", pending)
	}

	// Every way of acting on it now refuses, with the same answer.
	for name, act := range map[string]func() error{
		"approving":            func() error { return scope.Decide(ctx, requestID, userID, "approve", "") },
		"editing a statement":  func() error { return scope.EditStatement(ctx, userID, migrationID, false, 1, "SELECT 1;") },
		"writing a revert":     func() error { return scope.WriteRevert(ctx, userID, migrationID, "SELECT 1;") },
		"declaring it one-way": func() error { return scope.DeclareIrreversible(ctx, userID, migrationID, "because") },
		"starting a thread":    func() error { _, err := scope.StartThread(ctx, requestID, userID, "", "hello"); return err },
		"queueing execution":   func() error { return scope.EnqueueExecution(ctx, userID, requestID) },
		"closing it again":     func() error { return scope.CloseRequest(ctx, userID, requestID, "") },
	} {
		if err := act(); err == nil {
			t.Errorf("%s was allowed on a closed request", name)
		}
	}
}

// TestVerdictsFreezeOnceAMigrationHasRun covers the behaviour a merged pull
// request has: the review controls go, the conversation stays.
//
// A review is advice given before a thing happens. Once a migration is queued
// the advice has been taken; once it has run, approving it is a comment on the
// past dressed as a gate, and rejecting it does not un-run it. Decide had no
// state check at all, so a completed migration could still be approved.
func TestVerdictsFreezeOnceAMigrationHasRun(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the verdict test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, userID, target, source int64
	if err := pool.QueryRow(ctx, `
		SELECT i.project_id,
		       (SELECT m.user_id FROM schemaver.project_member m
		         WHERE m.project_id = i.project_id AND m.role = 'admin' LIMIT 1),
		       d.id, (SELECT id FROM schemaver.database WHERE name = 'shop_staging')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.name = 'shop_prod' AND d.current_fingerprint IS NOT NULL`).
		Scan(&projectID, &userID, &target, &source); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	requestID, err := scope.Propose(ctx, userID, target, source, "verdict probe", "")
	if err != nil {
		t.Skipf("nothing to migrate: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.change_request WHERE id = $1`, requestID)
	})
	if _, err := scope.GenerateMigration(ctx, userID, requestID); err != nil {
		t.Skipf("nothing to generate: %v", err)
	}

	// While it is under review, a verdict is accepted.
	if err := scope.Decide(ctx, requestID, userID, "approve", "looks right"); err != nil {
		t.Fatalf("approving a request under review: %v", err)
	}

	// Past review, it is not — in any of the states a request reaches after
	// somebody has committed to running it.
	for _, state := range []string{
		"READY_TO_EXECUTE", "EXECUTING", "COMPLETED", "NEEDS_ATTENTION", "CLOSED",
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE schemaver.change_request SET state = $2 WHERE id = $1`,
			requestID, state); err != nil {
			t.Fatalf("set %s: %v", state, err)
		}
		if err := scope.Decide(ctx, requestID, userID, "reject", "changed my mind"); err == nil {
			t.Errorf("a verdict was accepted on a %s request; rejecting it does "+
				"not un-run it, and approving it is a comment on the past", state)
		}
	}
}
