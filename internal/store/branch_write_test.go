package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/store"
)

// TestAWriteInFlightSaysSo covers the gap between pressing the button and
// finding out.
//
// A write is applied to a throwaway database and the result read back, which
// takes seconds. The button cannot report the outcome, so the page after it has
// to say that something is happening — otherwise it is identical to the page
// before, the press reads as having done nothing, and the only evidence arrives
// minutes later on a screen nobody is looking at.
func TestAWriteInFlightSaysSo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)

	st := store.New(pool, nil)
	projectID, userID, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	branchID, err := scope.CutBranch(ctx, userID, databaseID, "in-flight", "")
	if err != nil {
		t.Fatalf("CutBranch: %v", err)
	}

	before, err := scope.Branch(ctx, branchID)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if before.Applying {
		t.Fatal("a branch nobody has written to reports work in flight")
	}

	const sql = "ALTER TABLE public.orders ADD COLUMN channel text;"
	if err := scope.WriteToBranch(ctx, userID, branchID, sql, "adding channel"); err != nil {
		t.Fatalf("WriteToBranch: %v", err)
	}

	// The worker has not run yet, which is exactly the moment the page is shown.
	during, err := scope.Branch(ctx, branchID)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if !during.Applying {
		t.Error("the page cannot tell that a write is in flight, so pressing " +
			"the button leads to a page identical to the one before it")
	}
	if !strings.Contains(during.Pending, "channel") {
		t.Errorf("the statements being applied are not available to show: %q",
			during.Pending)
	}
	if during.WriteError != "" {
		t.Errorf("an error before anything ran: %q", during.WriteError)
	}
}
