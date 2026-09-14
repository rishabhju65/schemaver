package store

import (
	"context"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/introspect"
)

// TableSize is what a table costs to touch.
type TableSize struct {
	Namespace string
	Table     string
	Bytes     int64
	Rows      int64
	Analysed  bool
}

// Qualified is the table's path, matching how a change names its subject.
func (t TableSize) Qualified() string { return t.Namespace + "." + t.Table }

// Empty reports a table with no rows, which is only knowable where it has been
// analysed. An unanalysed table is not empty; it is unmeasured, and the two
// must not answer the same question the same way.
func (t TableSize) Empty() bool { return t.Analysed && t.Rows == 0 }

// Describe renders the size the way a warning would read it.
//
// The unanalysed case gets its own words rather than a number, because the
// honest answer is that nobody knows — and a plausible-looking zero is worse
// than an admission.
func (t TableSize) Describe() string {
	size := humanBytes(t.Bytes)
	if !t.Analysed {
		return size + ", never analysed so the row count is unknown"
	}
	if t.Rows == 0 {
		return size + ", empty"
	}
	return fmt.Sprintf("%s, about %s rows", size, humanCount(t.Rows))
}

// Big reports a table large enough that what a change does to it matters more
// than what the change is.
//
// A threshold rather than a measurement, and deliberately generous: the point
// is to separate "this rewrites something substantial" from "this rewrites
// nothing", not to predict a duration. Anything that claims to predict one from
// a byte count is guessing at hardware it cannot see.
func (t TableSize) Big() bool {
	const substantial = 1 << 30 // a gigabyte on disk
	const many = 10_000_000
	return t.Bytes >= substantial || (t.Analysed && t.Rows >= many)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fbn", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// RecordSizes stores what each table currently costs.
//
// Replaced wholesale rather than merged: a table that has gone should not leave
// its last known size behind to be read as current, and the set is small enough
// that working out which rows to keep would cost more than writing them all.
func (s *Store) RecordSizes(ctx context.Context, databaseID int64, sizes []introspect.TableSize) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM schemaver.table_size WHERE database_id = $1`, databaseID); err != nil {
		return fmt.Errorf("clear previous sizes: %w", err)
	}
	for _, t := range sizes {
		var rows *int64
		if t.Analysed {
			r := t.Rows
			rows = &r
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO schemaver.table_size
			    (database_id, namespace, table_name, bytes, rows_estimate, analysed)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			databaseID, t.Namespace, t.Table, t.Bytes, rows, t.Analysed); err != nil {
			return fmt.Errorf("record size of %s.%s: %w", t.Namespace, t.Table, err)
		}
	}
	return tx.Commit(ctx)
}

// Sizes reads what each table on a database costs, keyed by qualified name so a
// change can be looked up by the subject it already names.
func (s *Scope) Sizes(ctx context.Context, databaseID int64) (map[string]TableSize, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT t.namespace, t.table_name, t.bytes, t.rows_estimate, t.analysed
		  FROM schemaver.table_size t
		  JOIN schemaver.database d ON d.id = t.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE t.database_id = $1 AND i.project_id = ANY($2)`, databaseID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read table sizes: %w", err)
	}
	defer rows.Close()

	out := map[string]TableSize{}
	for rows.Next() {
		var t TableSize
		var estimate *int64
		if err := rows.Scan(&t.Namespace, &t.Table, &t.Bytes, &estimate, &t.Analysed); err != nil {
			return nil, fmt.Errorf("scan table size: %w", err)
		}
		if estimate != nil {
			t.Rows = *estimate
		}
		out[t.Qualified()] = t
	}
	return out, rows.Err()
}

// Cost pairs a change with the table it will do it to.
type Cost struct {
	Change diff.Change
	Size   TableSize
	// Known is false where nothing has been measured for that table — a change
	// to something never observed, or to a table this change is creating.
	Known bool
}

// Costly reports the changes on this migration whose cost depends on how much
// is there, paired with how much is there.
//
// Only the classes where it matters. Adding a nullable column is instant on any
// table, and saying "this touches a table with four hundred million rows" about
// it would train somebody to ignore the sentence by the time it was true.
func (d *RequestDetail) Costly() []Cost {
	var out []Cost
	for _, c := range d.ByRisk {
		if c.Class != diff.Rewriting && c.Class != diff.LockHeavy &&
			c.Class != diff.Destructive {
			continue
		}
		if c.Table == "" {
			continue
		}
		size, known := d.Sizes[c.Namespace+"."+c.Table]
		if known && !size.Big() {
			continue
		}
		out = append(out, Cost{Change: c, Size: size, Known: known})
	}
	return out
}
