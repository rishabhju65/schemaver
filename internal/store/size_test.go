package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestNeverAnalysedIsNotEmpty is the distinction the whole thing turns on.
//
// PostgreSQL writes -1 into reltuples for a table it has never analysed. A
// table nobody has analysed is usually a table nobody has looked at, which is
// exactly where an unpleasant surprise lives — and recording it as zero rows
// would turn "we do not know" into the most reassuring answer available.
func TestNeverAnalysedIsNotEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	if err := st.RecordSizes(ctx, databaseID, []introspect.TableSize{
		{Namespace: "public", Table: "measured", Bytes: 4096, Rows: 0, Analysed: true},
		{Namespace: "public", Table: "unmeasured", Bytes: 900 << 30, Analysed: false},
	}); err != nil {
		t.Fatalf("RecordSizes: %v", err)
	}

	sizes, err := scope.Sizes(ctx, databaseID)
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}

	measured := sizes["public.measured"]
	if !measured.Empty() {
		t.Error("a table analysed at zero rows is empty and should say so")
	}

	unmeasured := sizes["public.unmeasured"]
	if unmeasured.Empty() {
		t.Error("a table nobody has analysed was reported as empty; that is the " +
			"one wrong answer this distinction exists to prevent")
	}
	if unmeasured.Analysed {
		t.Error("an unanalysed table is reported as analysed")
	}
	if !strings.Contains(unmeasured.Describe(), "unknown") {
		t.Errorf("the description should admit it does not know, got %q",
			unmeasured.Describe())
	}
	// And it is still recognised as substantial, because the bytes are known
	// even when the rows are not.
	if !unmeasured.Big() {
		t.Error("900 GB is not treated as substantial because nobody analysed it")
	}
}

// TestSizeIsNotPartOfTheSchema. A table growing is not a schema change, and if
// it were, every observation of an unchanged database would be a new version.
func TestSizeIsNotPartOfTheSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	_ = projectID

	var before string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&before)

	if err := st.RecordSizes(ctx, databaseID, []introspect.TableSize{
		{Namespace: "public", Table: "orders", Bytes: 500 << 30, Rows: 400_000_000, Analysed: true},
	}); err != nil {
		t.Fatalf("RecordSizes: %v", err)
	}

	var after string
	pool.QueryRow(ctx, `SELECT current_fingerprint FROM schemaver.database WHERE id = $1`,
		databaseID).Scan(&after)
	if before != after {
		t.Errorf("recording a size moved the schema fingerprint: %s → %s",
			before[:12], after[:12])
	}
}

// TestSizesAreReplacedNotMerged: a table that has gone must not leave its last
// known size behind to be read as current.
func TestSizesAreReplacedNotMerged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := mergeTestPool(ctx, t)
	st := store.New(pool, nil)

	projectID, _, databaseID := branchFixture(ctx, t, pool, table(text("id")))
	scope := st.ForProject(projectID)

	st.RecordSizes(ctx, databaseID, []introspect.TableSize{
		{Namespace: "public", Table: "gone", Bytes: 1 << 30, Analysed: true, Rows: 1},
		{Namespace: "public", Table: "stays", Bytes: 4096, Analysed: true},
	})
	st.RecordSizes(ctx, databaseID, []introspect.TableSize{
		{Namespace: "public", Table: "stays", Bytes: 8192, Analysed: true},
	})

	sizes, err := scope.Sizes(ctx, databaseID)
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if _, still := sizes["public.gone"]; still {
		t.Error("a table that is no longer there kept its last known size")
	}
	if sizes["public.stays"].Bytes != 8192 {
		t.Errorf("the surviving table kept a stale size: %d", sizes["public.stays"].Bytes)
	}
}

// TestOnlyCostlyChangesAreCalledOut. Saying "this touches a table with four
// hundred million rows" about an added nullable column would train somebody to
// stop reading the sentence by the time it was true.
func TestOnlyCostlyChangesAreCalledOut(t *testing.T) {
	big := store.TableSize{Namespace: "public", Table: "orders",
		Bytes: 500 << 30, Rows: 400_000_000, Analysed: true}
	small := store.TableSize{Namespace: "public", Table: "flags",
		Bytes: 4096, Analysed: true}
	if !big.Big() {
		t.Fatal("500 GB is not substantial")
	}
	if small.Big() {
		t.Fatal("4 kB is substantial")
	}
	if !strings.Contains(big.Describe(), "400.0m rows") {
		t.Errorf("a row count should read as a count, got %q", big.Describe())
	}
	if !strings.Contains(big.Describe(), "GB") {
		t.Errorf("a size should read as a size, got %q", big.Describe())
	}
}
