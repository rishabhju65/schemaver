package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestClaimHonoursTheFraction pins down the per-instance budget arithmetic
// against a live metadata database.
//
// It exists because the fraction was once truncated to zero before it reached
// Postgres: the parameter sat in integer arithmetic, so it was typed as an
// integer, and 0.15 arrived as 0. Every instance then got its floor as its
// budget. Nothing failed and nothing was logged — light work still fit under
// the floor, so observation carried on as normal, while any migration heavier
// than the floor simply stopped being claimable. A test that only checked
// light work would have passed throughout, so this one claims work heavier
// than the floor.
//
// The probe uses a job kind no worker polls, so a running deployment cannot
// race it for the row.
func TestClaimHonoursTheFraction(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the budget test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered rather than deferred, and registered first: cleanups run after
	// the test function returns and in reverse order, so a deferred close would
	// shut the pool before the rows below could be removed.
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	const kind = "budget-probe"
	// Heavier than any floor used below, so admission can only come from the
	// fraction being applied to the headroom.
	const weight = 8

	// A throwaway instance of its own, rather than one the deployment is using:
	// a live worker rewrites an instance's sampled capacity on every observation
	// cycle, and its in-flight observations spend that instance's budget, both
	// of which would make this flaky.
	var projectID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM schemaver.project ORDER BY id LIMIT 1`).
		Scan(&projectID); err != nil {
		t.Skipf("no project to attach the probe to: %v", err)
	}

	var credentialID, instanceID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.credential (project_id, name, username, kind, secret_ref)
		VALUES ($1, 'budget-probe', 'probe', 'secret_ref', 'probe')
		RETURNING id`, projectID).Scan(&credentialID); err != nil {
		t.Fatalf("create probe credential: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.credential WHERE id = $1`, credentialID)
	})

	// 1000 connections of headroom, none reserved or in use, so the only thing
	// standing between the job and admission is the arithmetic.
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance
		       (project_id, name, host, credential_id,
		        max_connections, reserved_connections, used_connections)
		VALUES ($1, 'budget-probe', 'probe.invalid', $2, 1000, 0, 0)
		RETURNING id`, projectID, credentialID).Scan(&instanceID); err != nil {
		t.Fatalf("create probe instance: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
	})

	var jobID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.job (kind, instance_id, weight, run_after)
		VALUES ($1, $2, $3, now())
		RETURNING id`, kind, instanceID, weight).Scan(&jobID); err != nil {
		t.Fatalf("enqueue probe: %v", err)
	}

	// A fraction that leaves room for one connection must not admit work
	// weighing eight, whatever the headroom.
	stingy := store.Budget{Floor: 1, Ceiling: 32, Fraction: 0.001}
	if job, err := st.ClaimJob(ctx, "probe", time.Minute, stingy, []string{kind}); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("a budget of one admitted weight %d: job=%v err=%v", weight, job, err)
	}

	// The default fraction of 1000 connections of headroom is far more than
	// eight, so the same job must now be claimable.
	job, err := st.ClaimJob(ctx, "probe", time.Minute, store.DefaultBudget(), []string{kind})
	if err != nil {
		t.Fatalf("claim under the default budget: %v", err)
	}
	if job.ID != jobID {
		t.Fatalf("claimed job %d, want the probe %d", job.ID, jobID)
	}
}
