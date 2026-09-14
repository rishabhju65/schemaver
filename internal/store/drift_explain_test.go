package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestDriftSaysWhatDiffers covers a report that named a problem and not its
// subject.
//
// Drift compared two fingerprints and showed them. That says a database is not
// where it should be and nothing about what is wrong, so the only way to find
// out was to open a change request — backwards, since what changed is how
// somebody decides whether to open one. The comment in the drift package said
// as much: explaining a divergence "needs the semantic diff engine and does not
// exist yet". It exists.
func TestDriftSaysWhatDiffers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	// A peer to diverge from, and a divergence: one table gained a column and
	// another table appeared.
	var instanceID, peerID int64
	pool.QueryRow(ctx, `SELECT instance_id FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&instanceID)
	if err := pool.QueryRow(ctx, `
		INSERT INTO schemaver.database (instance_id, name, managed, current_fingerprint)
		VALUES ($1, 'drift_peer', true,
		        (SELECT current_fingerprint FROM schemaver.database WHERE id = $2))
		RETURNING id`, instanceID, databaseID).Scan(&peerID); err != nil {
		t.Fatalf("create the peer: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM schemaver.drift WHERE database_id = $1`, databaseID)
		pool.Exec(bg, `DELETE FROM schemaver.database WHERE id = $1`, peerID)
	})

	moveDatabase(ctx, t, pool, databaseID, withTables(
		tbl("orders", text("id"), text("channel")),
		tbl("coupon", text("id")),
	))

	var observed, expected string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&observed)
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		peerID).Scan(&expected)
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.drift (database_id, observed_fingerprint,
		    expected_fingerprint, expected_source, peer_database_id, status)
		VALUES ($1, $2, $3, 'peer', $4, 'open')`,
		databaseID, observed, expected, peerID); err != nil {
		t.Fatalf("record the drift: %v", err)
	}

	rows, err := scope.Drifts(ctx, false)
	if err != nil {
		t.Fatalf("Drifts: %v", err)
	}
	var found *store.DriftRow
	for i := range rows {
		if rows[i].DatabaseID == databaseID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("the divergence was not reported at all")
	}
	if found.Unexplained != "" {
		t.Fatalf("could not explain a divergence between two readable schemas: %s",
			found.Unexplained)
	}
	if len(found.Changed) == 0 {
		t.Fatal("the divergence is reported with no indication of what differs")
	}

	var names []string
	for _, c := range found.Changed {
		names = append(names, string(c.Kind)+" "+c.Name)
	}
	joined := strings.Join(names, "; ")
	if !strings.Contains(joined, "public.coupon") {
		t.Errorf("a table that exists on one side and not the other is not named: %s", joined)
	}
	if !strings.Contains(joined, "public.orders") {
		t.Errorf("a table whose columns differ is not named: %s", joined)
	}
	if found.Summary.Total() != len(found.Changed) {
		t.Errorf("the summary counts %d and the list has %d",
			found.Summary.Total(), len(found.Changed))
	}
	t.Logf("%s — %s", found.Summary.Describe(), joined)
}

// TestDriftAdmitsWhenItCannotExplain: an unreadable schema must not render as
// agreement, which is what an empty change list would look like.
func TestDriftAdmitsWhenItCannotExplain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.drift WHERE database_id = $1`, databaseID)
	})

	var observed string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&observed)
	// A schema that exists as bytes but that this project cannot reach: no
	// snapshot, no migration and no branch of its own names it. That is the
	// shape of a declared schema whose repository link has since gone — the
	// fingerprint is on the drift row and the schema behind it is not the
	// project's to read.
	orphan := storeSchema(ctx, t, pool, table(text("id"), text("gone")))
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.schema_blob WHERE fingerprint = $1`, orphan)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO schemaver.drift (database_id, observed_fingerprint,
		    expected_fingerprint, expected_source, status)
		VALUES ($1, $2, $3, 'declared', 'open')`,
		databaseID, observed, orphan); err != nil {
		t.Fatalf("record the drift: %v", err)
	}

	rows, err := scope.Drifts(ctx, false)
	if err != nil {
		t.Fatalf("Drifts: %v", err)
	}
	for _, r := range rows {
		if r.DatabaseID != databaseID {
			continue
		}
		if r.Unexplained == "" {
			t.Error("a divergence whose other side cannot be read was reported " +
				"without saying so; an empty change list reads as agreement")
		}
		if len(r.Changed) != 0 {
			t.Error("changes were reported from a schema that could not be read")
		}
		return
	}
	t.Fatal("the divergence was not reported")
}
