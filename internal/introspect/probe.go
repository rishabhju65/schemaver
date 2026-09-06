package introspect

import (
	"context"
	"fmt"
)

// Probe returns a digest of everything Schema would read, computed entirely
// inside the engine and returned as a single value.
//
// It exists so the common case costs one small round trip instead of seven
// queries returning thousands of rows. On an unchanged database the digest
// matches the stored one and no full read happens at all.
//
// What it saves is network transfer and client-side parsing, not catalog
// scanning — the engine still walks the same catalogs. That is still the bulk of
// the cost on a large schema.
//
// # This digest is not a fingerprint
//
// It is computed over raw catalog fields rather than the canonical model, so it
// is only ever compared against a previous value of itself. It must never be
// stored as identity, shown to a user as a version, or compared across
// databases: two databases with identical schemas can produce different probe
// digests, because the projections include engine-assigned detail.
//
// # The coupling this creates
//
// Probe must cover every field Schema reads. Add something to the model, forget
// it here, and changes to it become invisible — a silent staleness bug rather
// than a loud one. The mitigation is not vigilance: it is running a full
// unconditional read on a slower cadence, so any blind spot self-heals within
// one cycle instead of never.
func Probe(ctx context.Context, db Beginner) (string, error) {
	var digest string
	err := snapshot(ctx, db, func(q Querier) error {
		d, err := probe(ctx, q)
		digest = d
		return err
	})
	return digest, err
}

func probe(ctx context.Context, q Querier) (string, error) {
	rows, err := q.Query(ctx, probeSQL)
	if err != nil {
		return "", fmt.Errorf("probe schema: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("probe schema: %w", err)
		}
		return "", fmt.Errorf("probe schema: engine returned no row")
	}
	var digest string
	if err := rows.Scan(&digest); err != nil {
		return "", fmt.Errorf("scan probe digest: %w", err)
	}
	return digest, rows.Err()
}

// probeSQL projects every catalog field Schema reads into one text column and
// hashes the sorted result.
//
// Constraints and indexes are projected through pg_get_constraintdef and
// pg_get_indexdef rather than field by field: the engine's own deparser captures
// operator classes, sort order and opclass parameters that would be tedious to
// enumerate and easy to leave out.
var probeSQL = `
SELECT COALESCE(md5(string_agg(x, E'\n' ORDER BY x)), '') FROM (

    SELECT concat_ws(':', 'ns', n.nspname, COALESCE(d.description, '')) AS x
    FROM pg_namespace n
    LEFT JOIN pg_description d
      ON d.objoid = n.oid AND d.classoid = 'pg_namespace'::regclass
    WHERE ` + notSystem + ` AND ` + extensionOwnedNS + `

    UNION ALL
    SELECT concat_ws(':', 'rel', n.nspname, c.relname, c.relkind,
                     COALESCE(pg_get_partkeydef(c.oid), ''),
                     COALESCE(d.description, ''))
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_description d
      ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
    WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
      AND ` + notSystem + ` AND ` + extensionOwnedClass + `

    UNION ALL
    SELECT concat_ws(':', 'col', n.nspname, c.relname, a.attname,
                     format_type(a.atttypid, a.atttypmod), a.attnotnull,
                     COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''),
                     COALESCE(a.attidentity, ''), COALESCE(a.attgenerated, ''),
                     a.attcollation, COALESCE(d.description, ''))
    FROM pg_attribute a
    JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
    LEFT JOIN pg_description d
      ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = a.attnum
    WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
      AND a.attnum > 0 AND NOT a.attisdropped
      AND ` + notSystem + `

    UNION ALL
    SELECT concat_ws(':', 'con', n.nspname, c.relname, con.conname, con.contype,
                     pg_get_constraintdef(con.oid))
    FROM pg_constraint con
    JOIN pg_class c ON c.oid = con.conrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE con.contype IN ('p', 'f', 'u', 'c', 'x')
      AND ` + notSystem + ` AND ` + extensionOwnedConstraint + `

    UNION ALL
    SELECT concat_ws(':', 'idx', n.nspname, c.relname, ic.relname,
                     pg_get_indexdef(i.indexrelid), COALESCE(d.description, ''))
    FROM pg_index i
    JOIN pg_class ic ON ic.oid = i.indexrelid
    JOIN pg_class c ON c.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_description d
      ON d.objoid = ic.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
    WHERE NOT c.relispartition
      AND NOT EXISTS (SELECT 1 FROM pg_constraint con
                      WHERE con.conindid = i.indexrelid
                        AND con.contype IN ('p', 'u', 'x'))
      AND ` + notSystem + ` AND ` + extensionOwnedIndex + `

    UNION ALL
    SELECT concat_ws(':', 'enum', n.nspname, t.typname, e.enumsortorder, e.enumlabel)
    FROM pg_type t
    JOIN pg_enum e ON e.enumtypid = t.oid
    JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE ` + notSystem + ` AND ` + extensionOwnedType + `

    UNION ALL
    SELECT concat_ws(':', 'seq', n.nspname, c.relname, s.seqtypid, s.seqstart,
                     s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
                     COALESCE(d.description, ''))
    FROM pg_sequence s
    JOIN pg_class c ON c.oid = s.seqrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_description d
      ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
    WHERE ` + notSystem + ` AND ` + extensionOwnedClass + `

) probe`
