package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAnObjectHasItsOwnHistory is the view nothing in this space has.
//
// A snapshot holds a whole schema, so what happened to any one table inside it
// was already recorded; what was missing was a way to ask. Git works the same
// way — there is no per-file history object in a repository, and `git log --
// path` walks the commits and keeps the ones whose diff touches that path.
func TestAnObjectHasItsOwnHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, withTables(
		tbl("orders", text("id")),
		tbl("customers", text("id")),
	))
	scope := st.ForProject(projectID)

	// orders changes twice; customers changes once in between. A history of
	// orders must show two entries, not three.
	moveDatabase(ctx, t, pool, databaseID, withTables(
		tbl("orders", text("id"), text("channel")),
		tbl("customers", text("id")),
	))
	moveDatabase(ctx, t, pool, databaseID, withTables(
		tbl("orders", text("id"), text("channel")),
		tbl("customers", text("id"), text("email")),
	))
	moveDatabase(ctx, t, pool, databaseID, withTables(
		tbl("orders", text("id"), text("channel"), text("note")),
		tbl("customers", text("id"), text("email")),
	))

	events, err := scope.ObjectHistory(ctx, databaseID, "public.orders")
	if err != nil {
		t.Fatalf("ObjectHistory: %v", err)
	}
	if len(events) != 2 {
		var got []string
		for _, e := range events {
			got = append(got, string(e.Kind)+" at "+e.At.Format(time.TimeOnly))
		}
		t.Fatalf("expected the two transitions that touched orders, got %d: %s",
			len(events), strings.Join(got, "; "))
	}
	for _, e := range events {
		if len(e.Changes) == 0 {
			t.Error("a transition is reported with nothing said about what changed")
		}
		for _, c := range e.Changes {
			if c.Table != "orders" {
				t.Errorf("a change to %s.%s appears in the history of orders",
					c.Namespace, c.Table)
			}
		}
	}

	// And the other table's history is its own.
	theirs, err := scope.ObjectHistory(ctx, databaseID, "public.customers")
	if err != nil {
		t.Fatalf("ObjectHistory: %v", err)
	}
	if len(theirs) != 1 {
		t.Errorf("customers changed once; its history has %d entries", len(theirs))
	}
}

// TestAnObjectHistorySaysWhatNothingAccountsFor is the half a schema file
// cannot answer: a table that changed with no change request behind it was
// changed outside the product, which is exactly what drift means.
func TestAnObjectHistorySaysWhatNothingAccountsFor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)
	moveDatabase(ctx, t, pool, databaseID, table(text("id"), text("appeared")))

	events, err := scope.ObjectHistory(ctx, databaseID, "public.orders")
	if err != nil {
		t.Fatalf("ObjectHistory: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("a change to the table is not in its history")
	}
	for _, e := range events {
		if e.Caused() {
			t.Errorf("a change nobody requested is attributed to request %d", e.RequestID)
		}
	}
}

// TestObjectsListsWhatIsThere, so a reader has something to pick from rather
// than needing to know a name to type.
func TestObjectsListsWhatIsThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, withTables(
		tbl("orders", text("id")),
		tbl("customers", text("id")),
	))
	objects, err := st.ForProject(projectID).Objects(ctx, databaseID)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	var names []string
	for _, o := range objects {
		names = append(names, o.Name)
	}
	for _, want := range []string{"public.orders", "public.customers"} {
		var found bool
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not listed; got %v", want, names)
		}
	}
}
