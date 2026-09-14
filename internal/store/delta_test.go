package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestADeltaAnswersBothQuestions covers the ordering a reader actually needs:
// which objects this touches, before what it does to them.
//
// A list of statements answers the second question well and the first not at
// all, and the first is the one that decides whether to keep reading.
func TestADeltaAnswersBothQuestions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, _ := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	before := storeSchema(ctx, t, pool, withTables(
		tbl("orders", text("id"), text("legacy")),
		tbl("audit_log", text("id")),
	))
	after := storeSchema(ctx, t, pool, withTables(
		tbl("orders", text("id"), text("channel")),
		tbl("coupon", text("id")),
	))
	// Reachable: Between reads through the scoped reader, so the schemas have
	// to be this project's to compare.
	reachable(ctx, t, pool, projectID, before, after)

	d, err := scope.Between(ctx, schemaVersion(before), schemaVersion(after))
	if err != nil {
		t.Fatalf("Between: %v", err)
	}

	var objects []string
	for _, o := range d.Objects {
		objects = append(objects, string(o.Kind)+" "+o.Name)
	}
	joined := strings.Join(objects, "; ")
	for _, want := range []string{"public.coupon", "public.audit_log", "public.orders"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s is not in the object list: %s", want, joined)
		}
	}
	if d.Summary.Total() != len(d.Objects) {
		t.Errorf("summary counts %d, list has %d", d.Summary.Total(), len(d.Objects))
	}

	// And the operations are there too, at the finer grain.
	if len(d.Changes) == 0 {
		t.Fatal("a delta with objects and no changes describes nothing")
	}

	// Picking one object out of the summary gives what happened inside it,
	// without the rest.
	inOrders := d.ObjectsIn("public.orders")
	if len(inOrders) == 0 {
		t.Fatal("no changes attributed to public.orders")
	}
	for _, c := range inOrders {
		if c.Table != "orders" {
			t.Errorf("a change to %s.%s was attributed to public.orders",
				c.Namespace, c.Table)
		}
	}
	t.Logf("%s — %s (%d operations)", d.Summary.Describe(), joined, len(d.Changes))
}

// TestADeltaRefusesAnUnreadableSchema: comparing against something this project
// cannot read is an error, not an empty result that reads as agreement.
func TestADeltaRefusesAnUnreadableSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	var mine string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&mine)
	orphan := storeSchema(ctx, t, pool, table(text("id"), text("unreachable")))
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM schemaver.schema_blob WHERE fingerprint = $1`, orphan)
	})

	if _, err := scope.Between(ctx, schemaVersion(mine), schemaVersion(orphan)); err == nil {
		t.Error("compared against a schema this project cannot read, and reported " +
			"no difference rather than saying so")
	}
}
