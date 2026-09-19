package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAQueuedRunThatCannotStartSaysWhy is the case "queued" gets wrong.
//
// An execute job is claimable only while the plan's starting point still
// matches the database. That is deliberate — a migration queued behind another
// waits instead of failing its precondition and retrying — but the same
// condition can be permanently false, and a page that says "waiting for a
// worker, usually moments" is then saying the opposite of what is true.
func TestAQueuedRunThatCannotStartSaysWhy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "stuck-run", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}
	advance(ctx, t, pool, branchID, table(text("id"), text("channel")))
	requestID, err := scope.MergeBranch(ctx, userID, branchID, databaseID, "stuck", "")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}

	// Queue a run without involving a worker, which is the state right after
	// somebody presses the button.
	var migrationID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM schemaver.migration
		 WHERE change_request_id = $1 AND superseded_at IS NULL`,
		requestID).Scan(&migrationID); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, database_id, weight, idempotency_key)
		VALUES ('execute', 'migration', $1, $2, 1, $3)`,
		migrationID, databaseID, "execute-stuck-probe"); err != nil {
		t.Fatalf("queue the run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE schemaver.change_request SET state = 'READY_TO_EXECUTE' WHERE id = $1`,
		requestID); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	// Nothing wrong yet: it is genuinely just waiting.
	d, err := scope.Request(ctx, requestID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !d.Queued() {
		t.Fatal("not reported as queued")
	}
	if d.RunBlocked != "" {
		t.Errorf("reported as blocked while it is merely waiting: %q", d.RunBlocked)
	}
	if d.Stuck() {
		t.Error("a run that can still start is reported as stuck")
	}
	if !d.Working() {
		t.Error("the page would not follow a run that is about to start")
	}

	// The database moves. The job's precondition is now permanently false.
	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("elsewhere")))

	d, err = scope.Request(ctx, requestID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if d.RunBlocked == "" {
		t.Fatal("a run that can never start is still shown as waiting for a " +
			"worker, which is the opposite of what is true")
	}
	if !strings.Contains(d.RunBlocked, "moved") {
		t.Errorf("the reason does not say the database moved: %q", d.RunBlocked)
	}
	if !d.Stuck() {
		t.Error("not reported as stuck")
	}
	if d.Working() {
		t.Error("the page would keep reloading forever waiting for something " +
			"that will not happen")
	}
}
