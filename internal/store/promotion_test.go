package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestProductionWaitsForTheEnvironmentBelowIt is D-010's rule, checked against a
// live deployment: a change cannot run where it has not already run one
// environment down.
//
// The question "has this been through staging" is asked as "is staging already
// at the schema this migration targets", which is why nothing has to remember
// that a rehearsal happened. This test exercises both directions of that: a
// migration aiming somewhere staging has not reached is refused, and the same
// migration becomes executable the moment staging arrives there — with nothing
// re-approved in between.
func TestProductionWaitsForTheEnvironmentBelowIt(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the promotion test")
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

	// Restore whatever the deployment had, so this test does not reconfigure it.
	var wasPeer *int64
	pool.QueryRow(ctx, `SELECT expected_peer_id FROM schemaver.database WHERE id = $1`,
		target).Scan(&wasPeer)
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
			target, wasPeer)
	})
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $2 WHERE id = $1`,
		target, source); err != nil {
		t.Fatalf("point production at staging: %v", err)
	}

	requestID, err := scope.Propose(ctx, userID, target, source, "promotion probe", "")
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

	// Everything else satisfied, so only the promotion rule is under test.
	if err := scope.WriteRevert(ctx, userID, migrationID,
		"ALTER TABLE public.orders DROP COLUMN channel;"); err != nil {
		t.Fatalf("WriteRevert: %v", err)
	}
	if err := st.RecordProof(ctx, migrationID, "passed", "", nil); err != nil {
		t.Fatalf("RecordProof: %v", err)
	}
	if err := st.RecordRevertProof(ctx, migrationID, "passed", ""); err != nil {
		t.Fatalf("RecordRevertProof: %v", err)
	}
	if err := scope.Decide(ctx, requestID, userID, "approve", "ship it"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// This migration is generated from staging, so staging is already at its
	// target and the gate is open. Move staging away and it must shut.
	open, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if open.PromotionSource != "shop_staging" {
		t.Fatalf("promotion source is %q, want shop_staging", open.PromotionSource)
	}
	if !open.PromotionReached || !open.Executable {
		t.Fatalf("staging is at this migration's target and the gate is still shut: %s",
			open.Reason)
	}

	var realFingerprint string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		source).Scan(&realFingerprint)
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`UPDATE schemaver.database SET current_fingerprint = $2 WHERE id = $1`,
			source, realFingerprint)
	})
	var elsewhere string
	if err := pool.QueryRow(ctx, `
		SELECT fingerprint FROM schemaver.schema_blob
		 WHERE fingerprint <> $1 LIMIT 1`, realFingerprint).Scan(&elsewhere); err != nil {
		t.Skipf("need a second schema to move staging to: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET current_fingerprint = $2 WHERE id = $1`,
		source, elsewhere); err != nil {
		t.Fatalf("move staging: %v", err)
	}

	shut, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after moving staging: %v", err)
	}
	if shut.Executable {
		t.Error("the gate stayed open after the lower environment left the schema " +
			"this migration targets; the evidence did not expire")
	}
	if shut.PromotionReached {
		t.Error("staging is reported as having reached a schema it has left")
	}
	if !strings.Contains(shut.Reason, "shop_staging") {
		t.Errorf("the refusal does not name what it is waiting for: %s", shut.Reason)
	}
	t.Logf("shut: %s", shut.Reason)

	// And it opens again by itself, with nothing re-approved.
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET current_fingerprint = $2 WHERE id = $1`,
		source, realFingerprint); err != nil {
		t.Fatalf("return staging: %v", err)
	}
	again, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after returning staging: %v", err)
	}
	if !again.Executable {
		t.Errorf("the gate did not reopen when the lower environment came back: %s",
			again.Reason)
	}
}

// TestChangingWhatADatabaseFollows covers the two edits the mapping allows
// after onboarding, and what each should leave behind.
//
// The link is optional when a database is first managed and changeable
// afterwards, which means a deployment will re-point and unpair databases as it
// grows. Both leave records that described the old arrangement: an open
// divergence says "this does not match that", and after the edit "that" is no
// longer what this database is measured against.
func TestChangingWhatADatabaseFollows(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the mapping test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, instanceID, target, source int64
	if err := pool.QueryRow(ctx, `
		SELECT i.project_id, i.id, d.id,
		       (SELECT id FROM schemaver.database WHERE name = 'shop_staging')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.name = 'shop_prod'`).
		Scan(&projectID, &instanceID, &target, &source); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	var wasPeer *int64
	var wasEnv *int64
	pool.QueryRow(ctx, `
		SELECT expected_peer_id, environment_id FROM schemaver.database WHERE id = $1`,
		target).Scan(&wasPeer, &wasEnv)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `
			UPDATE schemaver.database SET expected_peer_id = $2, environment_id = $3
			 WHERE id = $1`, target, wasPeer, wasEnv)
	})

	settings := func(peer *int64) []store.DatabaseSettings {
		return []store.DatabaseSettings{{
			ID: target, Managed: true, EnvironmentID: wasEnv, PeerID: peer,
		}}
	}

	// Following something, and diverging from it.
	if err := scope.ApplyDatabaseSettings(ctx, instanceID, settings(&source)); err != nil {
		t.Fatalf("point at staging: %v", err)
	}
	var observed, expected string
	if err := pool.QueryRow(ctx, `
		SELECT min(fingerprint), max(fingerprint) FROM schemaver.schema_blob`).
		Scan(&observed, &expected); err != nil || observed == expected {
		t.Skipf("need two schemas to build a divergence: %v", err)
	}
	pool.Exec(ctx, `DELETE FROM schemaver.drift WHERE database_id = $1`, target)
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.drift
		       (database_id, observed_fingerprint, expected_fingerprint,
		        expected_source, peer_database_id)
		VALUES ($1, $2, $3, 'peer', $4)`,
		target, observed, expected, source); err != nil {
		t.Fatalf("create a divergence: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.drift WHERE database_id = $1`, target)
	})

	// Unpairing withdraws the expectation, so the divergence stops being one.
	if err := scope.ApplyDatabaseSettings(ctx, instanceID, settings(nil)); err != nil {
		t.Fatalf("unpair: %v", err)
	}
	var open int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schemaver.drift WHERE database_id = $1 AND status = 'open'`,
		target).Scan(&open); err != nil {
		t.Fatalf("count drift: %v", err)
	}
	if open != 0 {
		t.Errorf("%d divergence(s) still open against a database this one no longer "+
			"follows; a reader cannot tell that from a current one", open)
	}

	// Re-pointing asks for a fresh read rather than waiting out an interval,
	// because until one happens the page compares against the old predecessor.
	pool.Exec(ctx, `DELETE FROM schemaver.job WHERE kind = 'observe' AND target_id = $1`, target)
	if err := scope.ApplyDatabaseSettings(ctx, instanceID, settings(&source)); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job
		 WHERE kind = 'observe' AND target_id = $1 AND state = 'pending'`,
		target).Scan(&queued); err != nil {
		t.Fatalf("count queued reads: %v", err)
	}
	if queued == 0 {
		t.Error("re-pointing queued no read, so the comparison stays stale until " +
			"the next cycle comes round")
	}

	// Saving without changing the link asks for nothing.
	pool.Exec(ctx, `DELETE FROM schemaver.job WHERE kind = 'observe' AND target_id = $1`, target)
	if err := scope.ApplyDatabaseSettings(ctx, instanceID, settings(&source)); err != nil {
		t.Fatalf("save unchanged: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job
		 WHERE kind = 'observe' AND target_id = $1 AND state = 'pending'`,
		target).Scan(&queued); err != nil {
		t.Fatalf("count queued reads: %v", err)
	}
	if queued != 0 {
		t.Errorf("saving the page unchanged queued %d read(s); every visit to the "+
			"settings page would cost one", queued)
	}
}

// TestReadNowForcesAFullRead covers asking for a database to be read again,
// which somebody does after changing it outside schemaver.
//
// The distinction that matters is against the ordinary cycle: a scheduled read
// takes a cheap digest first and skips the full introspection when it decides
// nothing has changed. Somebody pressing this believes something has changed
// and wants to know, and the reason they are asking is often that they suspect
// the shortcut — so the digest is cleared and the next read is a full one.
func TestReadNowForcesAFullRead(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the read test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, userID, target int64
	if err := pool.QueryRow(ctx, `
		SELECT i.project_id,
		       (SELECT m.user_id FROM schemaver.project_member m
		         WHERE m.project_id = i.project_id AND m.role = 'admin' LIMIT 1),
		       d.id
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.name = 'shop_prod'`).Scan(&projectID, &userID, &target); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	// A digest present, as it would be after any ordinary read.
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET probe_digest = 'stale' WHERE id = $1`,
		target); err != nil {
		t.Fatalf("set a digest: %v", err)
	}
	pool.Exec(ctx, `DELETE FROM schemaver.job WHERE kind = 'observe' AND target_id = $1`, target)

	if err := scope.ReadNow(ctx, userID, target); err != nil {
		t.Fatalf("ReadNow: %v", err)
	}

	var digest *string
	if err := pool.QueryRow(ctx,
		`SELECT probe_digest FROM schemaver.database WHERE id = $1`, target).
		Scan(&digest); err != nil {
		t.Fatalf("read the digest back: %v", err)
	}
	if digest != nil {
		t.Errorf("the digest survived at %q, so the next read may take the "+
			"shortcut the reader is asking to bypass", *digest)
	}

	var queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.job
		 WHERE kind = 'observe' AND target_id = $1 AND state = 'pending'`,
		target).Scan(&queued); err != nil {
		t.Fatalf("count queued reads: %v", err)
	}
	if queued == 0 {
		t.Error("nothing was queued, so the read happens whenever the cycle " +
			"next comes round — which is what the reader is trying to avoid")
	}

	// An unmanaged database is not read on request, because it is not read at
	// all; saying so beats queueing work that will be skipped.
	var wasManaged bool
	pool.QueryRow(ctx, `SELECT managed FROM schemaver.database WHERE id = $1`,
		target).Scan(&wasManaged)
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`UPDATE schemaver.database SET managed = $2 WHERE id = $1`, target, wasManaged)
	})
	pool.Exec(ctx, `UPDATE schemaver.database SET managed = false WHERE id = $1`, target)
	if err := scope.ReadNow(ctx, userID, target); !errors.Is(err, store.ErrNotObservable) {
		t.Errorf("reading an unmanaged database: %v, want ErrNotObservable", err)
	}
}

// TestFollowingCannotFormALoop covers the arrangement that would jam the
// promotion gate without any error being raised.
//
// Each database waits for the one below it to reach a schema. In a loop every
// member is below every other, so none may go first: nothing fails, the
// requests simply never become executable, and the reason each one gives names
// a database that is itself waiting. Fan-out is a different matter and stays
// allowed — one staging database legitimately precedes several production ones.
func TestFollowingCannotFormALoop(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the loop test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, instanceID, credentialID int64
	if err := pool.QueryRow(ctx, `
		SELECT p.id, i.id, i.credential_id FROM schemaver.project p
		  JOIN schemaver.instance i ON i.project_id = p.id LIMIT 1`).
		Scan(&projectID, &instanceID, &credentialID); err != nil {
		t.Skipf("no fixtures: %v", err)
	}
	scope := st.ForProject(projectID)

	// Three databases of this test's own, so a real deployment's arrangement is
	// neither read nor disturbed.
	var probe int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, 'loop-probe', 'loop.invalid', $2) RETURNING id`,
		projectID, credentialID).Scan(&probe); err != nil {
		t.Skipf("create probe instance: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, probe)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, probe)
	})

	ids := map[string]int64{}
	for _, name := range []string{"dev", "stg", "prd"} {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed)
			VALUES ($1, $2, true) RETURNING id`, probe, name).Scan(&id); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		ids[name] = id
	}

	set := func(id int64, peer *int64) error {
		return scope.ApplyDatabaseSettings(ctx, probe,
			[]store.DatabaseSettings{{ID: id, Managed: true, PeerID: peer}})
	}
	stg, prd, dev := ids["stg"], ids["prd"], ids["dev"]

	// A chain is fine: prd follows stg follows dev.
	if err := set(stg, &dev); err != nil {
		t.Fatalf("stg follows dev: %v", err)
	}
	if err := set(prd, &stg); err != nil {
		t.Fatalf("prd follows stg: %v", err)
	}

	// Closing it is not, however long the way round.
	if err := set(dev, &prd); err == nil {
		t.Error("a three-database loop was accepted; every member waits for " +
			"every other and none may go first")
	} else if !strings.Contains(err.Error(), "loop") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if err := set(dev, &stg); err == nil {
		t.Error("a two-database loop was accepted")
	}

	// Fan-out stays allowed: one database may precede several.
	var second int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed)
		VALUES ($1, 'prd2', true) RETURNING id`, probe).Scan(&second); err != nil {
		t.Fatalf("create prd2: %v", err)
	}
	if err := set(second, &stg); err != nil {
		t.Errorf("two databases following the same one was refused: %v", err)
	}
}

// TestANewProjectGetsTwoEnvironments pins the shape a project starts with.
//
// Two, because two is what the product uses: a change reaches production
// through the environment below it, and staging is that environment. A third
// rung was somewhere to put a database and nothing more, and every extra
// concept in a tool people use occasionally is one more thing to work out
// before they can use it.
func TestANewProjectGetsTwoEnvironments(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the environment test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	org, project, user, err := st.CreateOrganization(ctx,
		"env-probe-org", "env-probe-project",
		"env-probe@example.test", "Env Probe", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.organization WHERE id = $1`, org.ID)
	})
	_ = user

	rows, err := pool.Query(ctx, `
		SELECT name, rank FROM schemaver.environment
		 WHERE project_id = $1 ORDER BY rank`, project.ID)
	if err != nil {
		t.Fatalf("read environments: %v", err)
	}
	defer rows.Close()

	var names []string
	var ranks []int
	for rows.Next() {
		var name string
		var rank int
		if err := rows.Scan(&name, &rank); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
		ranks = append(ranks, rank)
	}
	if len(names) != 2 {
		t.Fatalf("a new project got %d environments (%v), want 2", len(names), names)
	}
	if names[0] != "staging" || names[1] != "production" {
		t.Errorf("got %v in rank order, want staging then production", names)
	}
	// Order is the only thing rank means, and the gate reads it to tell a
	// promotion link pointing the right way from one pointing backwards.
	if ranks[0] >= ranks[1] {
		t.Errorf("staging ranks %d and production %d; staging must come first "+
			"or a change would be required to reach production before staging",
			ranks[0], ranks[1])
	}
}

// TestEitherAnswerAboutReversibilityOpensTheGate covers the choice D-025 put in
// place of a compulsory revert.
//
// The gate wants a decision, not a script. A change with a written way back and
// a change declared irreversible both satisfy it; a change where nobody has
// said either is refused, which is the state D-012 exists to prevent. The point
// of allowing the second answer is that a revert written only because a gate
// demanded one prevents nothing — it looks like a way back and is not one.
func TestEitherAnswerAboutReversibilityOpensTheGate(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the reversibility test")
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

	requestID, err := scope.Propose(ctx, userID, target, source, "reversibility probe", "")
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

	// Neither answer given: refused, and the refusal asks for a decision rather
	// than for a script.
	state, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState: %v", err)
	}
	if state.Executable {
		t.Error("executable with nobody having considered whether it can be undone")
	}
	if !strings.Contains(state.Reason, "undone") {
		t.Errorf("the refusal does not ask about reversibility: %s", state.Reason)
	}

	// An empty reason is not a decision.
	if err := scope.DeclareIrreversible(ctx, userID, migrationID, "   "); err == nil {
		t.Error("an empty reason was accepted; a reviewer agreeing to this needs " +
			"to know what they are agreeing to")
	}

	// Declaring it satisfies the gate.
	if err := scope.DeclareIrreversible(ctx, userID, migrationID,
		"drops the column and its contents; no DDL restores them"); err != nil {
		t.Fatalf("DeclareIrreversible: %v", err)
	}
	declared, err := scope.ApprovalState(ctx, requestID)
	if err != nil {
		t.Fatalf("ApprovalState after declaring: %v", err)
	}
	if declared.NoRevertReason == "" {
		t.Error("the declaration was not recorded")
	}
	if strings.Contains(declared.Reason, "undone") {
		t.Errorf("still asking about reversibility after it was settled: %s",
			declared.Reason)
	}

	// Writing one afterwards answers the other way, and the two must not both
	// stand: a script beside "there is no way back" is one somebody might run.
	if err := scope.WriteRevert(ctx, userID, migrationID,
		"ALTER TABLE public.orders DROP COLUMN channel;"); err != nil {
		t.Fatalf("WriteRevert after declaring: %v", err)
	}
	var reason *string
	var authored *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT no_revert_reason, revert_authored_at FROM schemaver.migration
		 WHERE id = $1`, migrationID).Scan(&reason, &authored); err != nil {
		t.Fatalf("read the migration back: %v", err)
	}
	if reason != nil {
		t.Error("a declaration of irreversibility survived beside a written revert")
	}
	if authored == nil {
		t.Error("the written revert was not recorded")
	}
}
