package introspect

import (
	"context"
	"fmt"
)

// TableSize is how much of a table there is.
//
// Deliberately not part of schema.Table, and the reason is identity. A schema's
// version is the digest of its canonical form, and a table growing is not a
// schema change: putting a size in the model would make every observation of an
// unchanged schema a new version, and every comparison between two databases
// report a difference because one has more rows than the other.
type TableSize struct {
	Namespace string
	Table     string

	// Bytes is the total on disk — the table, its indexes and its TOAST — which
	// is the number that predicts what a rewrite costs.
	Bytes int64

	// Rows is the planner's estimate, and Analysed reports whether there is one
	// at all.
	//
	// PostgreSQL writes -1 for a table it has never analysed, and the
	// difference between that and zero is the whole point of reading it. A
	// table nobody has analysed is usually a table nobody has looked at, which
	// is exactly where an unpleasant surprise lives; reporting it as empty
	// would turn "we do not know" into the most reassuring possible answer.
	Rows     int64
	Analysed bool
}

// Empty reports a table with no rows, which is only knowable where it has been
// analysed.
func (t TableSize) Empty() bool { return t.Analysed && t.Rows == 0 }

// Sizes reads how large each table is.
//
// Read from the catalogue rather than measured: pg_total_relation_size is a
// lookup of what the engine already tracks, and reltuples is whatever the last
// ANALYZE left behind. Neither scans anything, so this costs the same on a
// terabyte as on an empty database — which is the only reason it can run on
// every observation.
//
// A partitioned table is reported as the sum of its tree, because on its own it
// is nothing. The parent stores no rows, so pg_total_relation_size of it is
// zero however much is underneath — and a partitioned table reporting zero
// bytes is the worst possible answer, since tables get partitioned precisely
// because they are large. pg_partition_root collapses each partition onto its
// topmost parent, at any depth.
//
// The row estimate comes from the leaves and never from the parent. ANALYZE
// does populate a partitioned parent, but autovacuum does not process one, so
// in a database nobody has analysed by hand the parent reads -1 while every
// leaf underneath has a perfectly good estimate. Summing the leaves is right in
// both cases; trusting the parent is right only in one.
//
// Analysed is true only where every leaf has an estimate. A partial sum across
// a tree half of which has never been analysed is an undercount, and an
// undercount presented as a measurement is exactly the reassuring answer this
// whole field exists to refuse.
func Sizes(ctx context.Context, q Querier) ([]TableSize, error) {
	rows, err := q.Query(ctx, `
		WITH tree AS (
			SELECT COALESCE(pg_partition_root(c.oid), c.oid) AS root,
			       c.relkind,
			       pg_total_relation_size(c.oid) AS bytes,
			       c.reltuples
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relkind IN ('r', 'p')
			   AND `+notSystem+`
		)
		SELECT n.nspname, r.relname,
		       SUM(t.bytes),
		       SUM(CASE WHEN t.relkind = 'r' AND t.reltuples >= 0
		                THEN t.reltuples ELSE 0 END),
		       COALESCE(BOOL_AND(t.reltuples >= 0)
		                FILTER (WHERE t.relkind = 'r'), false)
		  FROM tree t
		  JOIN pg_class r ON r.oid = t.root
		  JOIN pg_namespace n ON n.oid = r.relnamespace
		 GROUP BY n.nspname, r.relname
		 ORDER BY n.nspname, r.relname`)
	if err != nil {
		return nil, fmt.Errorf("read table sizes: %w", err)
	}
	defer rows.Close()

	var out []TableSize
	for rows.Next() {
		var t TableSize
		var estimate float64
		if err := rows.Scan(&t.Namespace, &t.Table, &t.Bytes, &estimate,
			&t.Analysed); err != nil {
			return nil, fmt.Errorf("scan table size: %w", err)
		}
		if t.Analysed {
			t.Rows = int64(estimate)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
