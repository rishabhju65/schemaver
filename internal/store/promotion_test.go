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
