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

// TestRetireClosesWhatItPromises checks the two halves that must agree: what
// the confirmation page says retiring will close, and what retiring closes.
//
// They are computed by separate queries — one counting, one updating — which is
// exactly the arrangement that drifts. A confirmation that undercounts is worse
// than no confirmation, because it is believed.
//
// It builds its own instance and databases rather than using the deployment's,
// since retiring is destructive: it closes every open change request against a
// database, and restoring does not reopen them.
func TestRetireClosesWhatItPromises(t *testing.T) {
	url := os.Getenv("SCHEMAVER_METADATA_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_METADATA_URL to run the retirement test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool, nil)

	var projectID, userID int64
	if err := pool.QueryRow(ctx, `
		SELECT m.project_id, m.user_id FROM schemaver.project_member m
		 WHERE m.role = 'admin' ORDER BY m.project_id LIMIT 1`).
		Scan(&projectID, &userID); err != nil {
		t.Skipf("no project administrator to act as: %v", err)
	}
	scope := st.ForProject(projectID)

	var credentialID, instanceID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.credential (project_id, name, username, kind, secret_ref)
		VALUES ($1, 'retire-probe', 'probe', 'secret_ref', 'probe')
		RETURNING id`, projectID).Scan(&credentialID); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	// One cleanup in dependency order, closing over ids assigned below: an
	// instance cannot be deleted while its databases reference it, and a
	// credential cannot be deleted while an instance does.
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE instance_id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.instance WHERE id = $1`, instanceID)
		pool.Exec(bg, `DELETE FROM schemaver.credential WHERE id = $1`, credentialID)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.instance (project_id, name, host, credential_id)
		VALUES ($1, 'retire-probe', 'probe.invalid', $2) RETURNING id`,
		projectID, credentialID).Scan(&instanceID); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	// A database to retire, and a peer that compares itself against it.
	var target, peer int64
	for _, d := range []struct {
		name string
		into *int64
	}{{"probe_target", &target}, {"probe_peer", &peer}} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO schemaver.database (instance_id, name, managed)
			VALUES ($1, $2, true) RETURNING id`, instanceID, d.name).Scan(d.into); err != nil {
			t.Fatalf("create %s: %v", d.name, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE schemaver.database SET expected_peer_id = $1 WHERE id = $2`,
		target, peer); err != nil {
		t.Fatalf("pair peer: %v", err)
	}

	// One open change request, one open drift record, one queued job.
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.change_request
		       (project_id, title, author_id, database_id, source_database_id, state)
		VALUES ($1, 'probe request', $2, $3, $4, 'IN_REVIEW')`,
		projectID, userID, target, peer); err != nil {
		t.Fatalf("create request: %v", err)
	}
	// Real fingerprints: drift references schema_blob, so invented ones are
	// rejected.
	var observed, expected string
	if err := pool.QueryRow(ctx, `
		SELECT min(fingerprint), max(fingerprint) FROM schemaver.schema_blob`).
		Scan(&observed, &expected); err != nil || observed == expected {
		t.Skipf("need two distinct schema blobs to build a drift record: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.drift
		       (database_id, observed_fingerprint, expected_fingerprint,
		        expected_source, peer_database_id)
		VALUES ($1, $2, $3, 'peer', $4)`, target, observed, expected, peer); err != nil {
		t.Fatalf("create drift: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.job (kind, target_kind, target_id, instance_id, weight)
		VALUES ('observe', 'database', $1, $2, 1)`, target, instanceID); err != nil {
		t.Fatalf("create job: %v", err)
	}

	preview, err := scope.PreviewRetirement(ctx, target)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Quiet() {
		t.Fatal("the preview called a retirement quiet while work was outstanding")
	}

	done, err := scope.RetireDatabase(ctx, userID, target, "no longer in use")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if *done != *preview {
		t.Errorf("retiring did not do what the confirmation promised:\n  said: %+v\n  did:  %+v",
			*preview, *done)
	}
	if done.CancelledJobs != 1 || done.ClosedRequests != 1 ||
		done.ClosedDrift != 1 || done.UnpairedPeers != 1 {
		t.Errorf("cascade missed something: %+v", *done)
	}

	// Every write path refuses it, as target and as source.
	if _, err := scope.Propose(ctx, userID, target, peer, "x", ""); !errors.Is(err, store.ErrNotWritable) {
		t.Errorf("proposing against a retired database: %v, want ErrNotWritable", err)
	}
	if _, err := scope.Propose(ctx, userID, peer, target, "x", ""); !errors.Is(err, store.ErrNotWritable) {
		t.Errorf("proposing from a retired source: %v, want ErrNotWritable", err)
	}
	if _, err := scope.PreviewRetirement(ctx, target); !errors.Is(err, store.ErrNotWritable) {
		t.Errorf("retiring twice: %v, want ErrNotWritable", err)
	}

	// And it stays readable, which is the whole reason for retiring rather than
	// deleting.
	fleet, err := scope.Fleet(ctx)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	var found bool
	for _, d := range fleet {
		if d.ID != target {
			continue
		}
		found = true
		if d.Retired == nil || d.Writable {
			t.Errorf("retired row reads as retired=%v writable=%v", d.Retired != nil, d.Writable)
		}
		if d.RetiredReason != "no longer in use" {
			t.Errorf("reason came back as %q", d.RetiredReason)
		}
	}
	if !found {
		t.Error("a retired database vanished from the fleet; its history must stay readable")
	}
}
