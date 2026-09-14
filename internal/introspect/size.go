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
func Sizes(ctx context.Context, q Querier) ([]TableSize, error) {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname,
		       pg_total_relation_size(c.oid),
		       c.reltuples
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind IN ('r', 'p')
		   AND NOT c.relispartition
		   AND `+notSystem+`
		 ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, fmt.Errorf("read table sizes: %w", err)
	}
	defer rows.Close()

	var out []TableSize
	for rows.Next() {
		var t TableSize
		var estimate float64
		if err := rows.Scan(&t.Namespace, &t.Table, &t.Bytes, &estimate); err != nil {
			return nil, fmt.Errorf("scan table size: %w", err)
		}
		if estimate >= 0 {
			t.Rows, t.Analysed = int64(estimate), true
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
