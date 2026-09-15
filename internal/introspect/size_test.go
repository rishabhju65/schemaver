package introspect

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// sizeOf finds one table in the result, and reports whether it is there at all.
func sizeOf(sizes []TableSize, ns, table string) (TableSize, bool) {
	for _, s := range sizes {
		if s.Namespace == ns && s.Table == table {
			return s, true
		}
	}
	return TableSize{}, false
}

func sizes(t *testing.T, conn *pgx.Conn, ctx context.Context) []TableSize {
	t.Helper()
	got, err := Sizes(ctx, conn)
	if err != nil {
		t.Fatalf("sizes: %v", err)
	}
	return got
}

// TestAPartitionedTableIsTheSumOfItsTree is the bug this query was rewritten
// for.
//
// A partitioned parent stores no rows, so pg_total_relation_size of it is zero
// however much is underneath. Read on its own it reports as an empty table —
// and a table is partitioned in the first place because it is large, so the one
// case where the size matters most was the one case answered with nothing.
func TestAPartitionedTableIsTheSumOfItsTree(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "size_tree", `
		CREATE TABLE part (id bigint, d date, pad text) PARTITION BY RANGE (d);
		CREATE TABLE part_a PARTITION OF part
			FOR VALUES FROM ('2021-01-01') TO ('2022-01-01');
		CREATE TABLE part_b PARTITION OF part
			FOR VALUES FROM ('2022-01-01') TO ('2023-01-01');
		INSERT INTO part SELECT g, '2021-03-01', repeat('x', 100)
			FROM generate_series(1, 20000) g;
		INSERT INTO part SELECT g, '2022-03-01', repeat('x', 100)
			FROM generate_series(1, 20000) g;
		ANALYZE part_a;
		ANALYZE part_b;
	`)

	got := sizes(t, conn, ctx)

	parent, ok := sizeOf(got, "size_tree", "part")
	if !ok {
		t.Fatal("the partitioned table is missing from the result")
	}
	if parent.Bytes == 0 {
		t.Error("the partitioned table reports zero bytes, so every warning " +
			"that depends on how big it is will be suppressed")
	}
	if !parent.Analysed {
		t.Error("every partition was analysed, so the tree has a row estimate")
	}
	if parent.Rows < 39000 || parent.Rows > 41000 {
		t.Errorf("row estimate %d is not the ~40000 rows in the two partitions; "+
			"the leaves are either not summed or double counted", parent.Rows)
	}

	// The partitions are not tables in their own right here. They are storage
	// for one table, and listing them separately would report the same rows
	// twice and offer a subject no change ever names.
	for _, name := range []string{"part_a", "part_b"} {
		if _, listed := sizeOf(got, "size_tree", name); listed {
			t.Errorf("partition %s is listed as a table of its own", name)
		}
	}
}

// TestSubPartitionsRollUpToTheTopmostParent is why this uses
// pg_partition_root rather than a join to the immediate parent.
func TestSubPartitionsRollUpToTheTopmostParent(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "size_deep", `
		CREATE TABLE deep (id bigint, d date, pad text) PARTITION BY RANGE (d);
		CREATE TABLE deep_2021 PARTITION OF deep
			FOR VALUES FROM ('2021-01-01') TO ('2022-01-01') PARTITION BY RANGE (d);
		CREATE TABLE deep_2021_h1 PARTITION OF deep_2021
			FOR VALUES FROM ('2021-01-01') TO ('2021-07-01');
		INSERT INTO deep SELECT g, '2021-03-01', repeat('x', 100)
			FROM generate_series(1, 20000) g;
		ANALYZE deep_2021_h1;
	`)

	got := sizes(t, conn, ctx)

	root, ok := sizeOf(got, "size_deep", "deep")
	if !ok {
		t.Fatal("the root of the partition tree is missing from the result")
	}
	if root.Bytes == 0 {
		t.Error("the grandchild's storage was not attributed to the root")
	}
	if !root.Analysed || root.Rows < 19000 {
		t.Errorf("root reports analysed=%v rows=%d; the grandchild holds ~20000",
			root.Analysed, root.Rows)
	}
	for _, name := range []string{"deep_2021", "deep_2021_h1"} {
		if _, listed := sizeOf(got, "size_deep", name); listed {
			t.Errorf("%s is listed separately instead of rolling up", name)
		}
	}
}

// TestTheEstimateComesFromTheLeavesNotTheParent is the case a real database is
// almost always in.
//
// ANALYZE populates a partitioned parent, but autovacuum never processes one.
// So on a database nobody has analysed by hand the parent reads -1 while every
// leaf underneath has a good estimate, and reading the parent would report the
// largest tables in the database as unmeasured.
func TestTheEstimateComesFromTheLeavesNotTheParent(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "size_leaves", `
		CREATE TABLE only_leaves (id bigint, d date) PARTITION BY RANGE (d);
		CREATE TABLE only_leaves_a PARTITION OF only_leaves
			FOR VALUES FROM ('2021-01-01') TO ('2022-01-01');
		INSERT INTO only_leaves SELECT g, '2021-03-01'
			FROM generate_series(1, 20000) g;
		ANALYZE only_leaves_a;
	`)

	// The parent itself is untouched by the ANALYZE above, which is what makes
	// this test worth having.
	var parentEstimate float64
	if err := conn.QueryRow(ctx, `
		SELECT c.reltuples FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'size_leaves' AND c.relname = 'only_leaves'`).
		Scan(&parentEstimate); err != nil {
		t.Fatalf("read parent estimate: %v", err)
	}
	if parentEstimate >= 0 {
		t.Skip("this engine analysed the parent too, so there is nothing to prove")
	}

	got := sizes(t, conn, ctx)
	tbl, ok := sizeOf(got, "size_leaves", "only_leaves")
	if !ok {
		t.Fatal("the partitioned table is missing from the result")
	}
	if !tbl.Analysed {
		t.Fatal("reported as never analysed while its only leaf has an estimate")
	}
	if tbl.Rows < 19000 {
		t.Errorf("row estimate %d, expected ~20000 from the leaf", tbl.Rows)
	}
}

// TestOneUnanalysedLeafMakesTheWholeTreeUnmeasured keeps the honesty the
// unpartitioned case already has.
//
// A sum across a tree half of which has never been analysed is an undercount,
// and an undercount presented as a measurement is the reassuring answer this
// field exists to refuse.
func TestOneUnanalysedLeafMakesTheWholeTreeUnmeasured(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "size_partial", `
		CREATE TABLE partial (id bigint, d date) PARTITION BY RANGE (d);
		CREATE TABLE partial_a PARTITION OF partial
			FOR VALUES FROM ('2021-01-01') TO ('2022-01-01');
		CREATE TABLE partial_b PARTITION OF partial
			FOR VALUES FROM ('2022-01-01') TO ('2023-01-01');
		INSERT INTO partial SELECT g, '2021-03-01' FROM generate_series(1, 5000) g;
		INSERT INTO partial SELECT g, '2022-03-01' FROM generate_series(1, 5000) g;
		ANALYZE partial_a;
	`)

	got := sizes(t, conn, ctx)
	tbl, ok := sizeOf(got, "size_partial", "partial")
	if !ok {
		t.Fatal("the partitioned table is missing from the result")
	}
	if tbl.Analysed {
		t.Error("reported as measured while one partition has never been " +
			"analysed, so the row count is an undercount presented as a fact")
	}
	if tbl.Bytes == 0 {
		t.Error("bytes are known whether or not anything was analysed")
	}
}

// TestAnOrdinaryTableIsUnchanged is the regression guard. Every table in every
// database that is not partitioned has to answer exactly as it did before.
func TestAnOrdinaryTableIsUnchanged(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "size_plain", `
		CREATE TABLE measured (id bigint, pad text);
		INSERT INTO measured SELECT g, repeat('x', 100)
			FROM generate_series(1, 5000) g;
		ANALYZE measured;
		CREATE TABLE never_analysed (id bigint);
		CREATE TABLE empty_analysed (id bigint);
		ANALYZE empty_analysed;
	`)

	got := sizes(t, conn, ctx)

	m, ok := sizeOf(got, "size_plain", "measured")
	if !ok {
		t.Fatal("measured is missing")
	}
	if !m.Analysed || m.Rows < 4900 || m.Rows > 5100 || m.Bytes == 0 {
		t.Errorf("measured: analysed=%v rows=%d bytes=%d", m.Analysed, m.Rows, m.Bytes)
	}

	n, ok := sizeOf(got, "size_plain", "never_analysed")
	if !ok {
		t.Fatal("never_analysed is missing")
	}
	if n.Analysed {
		t.Error("never_analysed reports an estimate it does not have")
	}
	if n.Empty() {
		t.Error("never analysed is being read as empty, which is the one " +
			"answer that must never be given")
	}

	e, ok := sizeOf(got, "size_plain", "empty_analysed")
	if !ok {
		t.Fatal("empty_analysed is missing")
	}
	if !e.Analysed || !e.Empty() {
		t.Errorf("empty_analysed: analysed=%v empty=%v — an analysed table with "+
			"no rows is knowably empty", e.Analysed, e.Empty())
	}
}
