// Package introspect reads a live database into the canonical schema model.
//
// It is the only place that speaks pg_catalog. Everything it produces is fed
// through schema.Normalize, so a schema read from a database and the same schema
// read from anywhere else are byte-identical.
//
// Two things are deliberately excluded from the result:
//
//   - Objects owned by an extension. Installing an extension is not a schema
//     change the author made, and including its tables would make every diff
//     noisy and every drift report wrong.
//   - Indexes that exist to enforce a constraint. Those belong to the
//     constraint; recording them separately would make one logical object
//     appear twice in every diff.
package introspect

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Querier is the subset of pgx used here, so callers may pass a connection, a
// pool, or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Beginner is a connection that can start a transaction. Introspection needs one
// (see Schema).
type Beginner interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// snapshot runs fn against a transaction configured so that introspection is
// both deterministic and internally consistent.
//
// Two settings, each fixing a real defect:
//
//   - search_path is emptied. format_type renders a type as "st" or "d_a.st"
//     depending on whether its schema is in the search path, so without this the
//     same database yields different fingerprints depending on who connected —
//     which would break the one guarantee the canonical model exists to provide.
//     pg_catalog remains implicitly searched, so our own queries still resolve.
//   - REPEATABLE READ, so all seven catalog queries observe one instant. Without
//     it a schema changing mid-read produces a model that never existed.
func snapshot(ctx context.Context, db Beginner, fn func(Querier) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin introspection transaction: %w", err)
	}
	// Read-only work: rolling back is the correct end, and it cannot fail in a
	// way the caller needs to hear about.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		"SET LOCAL transaction_isolation = 'repeatable read'"); err != nil {
		return fmt.Errorf("set isolation: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = ''"); err != nil {
		return fmt.Errorf("clear search_path: %w", err)
	}
	return fn(tx)
}

// notSystem excludes system and temporary schemas, and anything an extension
// owns. Applied to every query so the filters cannot drift apart.
const notSystem = `
	  n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
	  AND n.nspname NOT LIKE 'pg\_temp\_%'
	  AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'`

func extensionOwned(class, oid string) string {
	return fmt.Sprintf(`NOT EXISTS (
		SELECT 1 FROM pg_depend dep
		WHERE dep.classid = '%s'::regclass AND dep.objid = %s AND dep.deptype = 'e')`, class, oid)
}

// Pre-built forms of the same filter, so probe.go can concatenate them into a
// package-level string constant.
var (
	extensionOwnedNS         = extensionOwned("pg_namespace", "n.oid")
	extensionOwnedClass      = extensionOwned("pg_class", "c.oid")
	extensionOwnedIndex      = extensionOwned("pg_class", "ic.oid")
	extensionOwnedConstraint = extensionOwned("pg_constraint", "con.oid")
	extensionOwnedType       = extensionOwned("pg_type", "t.oid")
)

// tableKey identifies a table across the several queries that build it up.
type tableKey struct{ ns, table string }

// Schema reads the complete schema visible to db and returns it in canonical
// form.
//
// It runs inside its own transaction so that type names are rendered
// deterministically and every catalog query sees the same instant; see snapshot.
func Schema(ctx context.Context, db Beginner) (*schema.Schema, error) {
	var out *schema.Schema
	err := snapshot(ctx, db, func(q Querier) error {
		s, err := readSchema(ctx, q)
		out = s
		return err
	})
	return out, err
}

func readSchema(ctx context.Context, q Querier) (*schema.Schema, error) {
	namespaces, order, err := readNamespaces(ctx, q)
	if err != nil {
		return nil, err
	}

	tables, tableOrder, err := readTables(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := readColumns(ctx, q, tables); err != nil {
		return nil, err
	}
	if err := readConstraints(ctx, q, tables); err != nil {
		return nil, err
	}
	if err := readIndexes(ctx, q, tables); err != nil {
		return nil, err
	}

	for _, k := range tableOrder {
		ns, ok := namespaces[k.ns]
		if !ok {
			// A table in a schema the namespace query excluded; skip rather than
			// inventing a namespace that was filtered for a reason.
			continue
		}
		ns.Tables = append(ns.Tables, *tables[k])
		namespaces[k.ns] = ns
	}

	if err := readSequences(ctx, q, namespaces); err != nil {
		return nil, err
	}
	if err := readEnums(ctx, q, namespaces); err != nil {
		return nil, err
	}

	out := &schema.Schema{}
	for _, name := range order {
		out.Namespaces = append(out.Namespaces, namespaces[name])
	}
	schema.Normalize(out)
	return out, nil
}

func readNamespaces(ctx context.Context, q Querier) (map[string]schema.Namespace, []string, error) {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, COALESCE(d.description, '')
		FROM pg_namespace n
		LEFT JOIN pg_description d
		  ON d.objoid = n.oid AND d.classoid = 'pg_namespace'::regclass
		WHERE `+notSystem+` AND `+extensionOwned("pg_namespace", "n.oid")+`
		ORDER BY n.nspname`)
	if err != nil {
		return nil, nil, fmt.Errorf("read namespaces: %w", err)
	}
	defer rows.Close()

	out := map[string]schema.Namespace{}
	var order []string
	for rows.Next() {
		var ns schema.Namespace
		if err := rows.Scan(&ns.Name, &ns.Comment); err != nil {
			return nil, nil, fmt.Errorf("scan namespace: %w", err)
		}
		out[ns.Name] = ns
		order = append(order, ns.Name)
	}
	return out, order, rows.Err()
}

func readTables(ctx context.Context, q Querier) (map[tableKey]*schema.Table, []tableKey, error) {
	// relispartition excludes partition children: they are produced by the
	// partitioning scheme of their parent, not declared independently.
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, c.relkind = 'p',
		       COALESCE(pg_get_partkeydef(c.oid), ''),
		       COALESCE(d.description, '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_description d
		  ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
		WHERE c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND `+notSystem+` AND `+extensionOwned("pg_class", "c.oid")+`
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, nil, fmt.Errorf("read tables: %w", err)
	}
	defer rows.Close()

	out := map[tableKey]*schema.Table{}
	var order []tableKey
	for rows.Next() {
		var k tableKey
		t := &schema.Table{}
		if err := rows.Scan(&k.ns, &t.Name, &t.Partitioned, &t.PartitionKey, &t.Comment); err != nil {
			return nil, nil, fmt.Errorf("scan table: %w", err)
		}
		k.table = t.Name
		out[k] = t
		order = append(order, k)
	}
	return out, order, rows.Err()
}

func readColumns(ctx context.Context, q Querier, tables map[tableKey]*schema.Table) error {
	// A collation is recorded only when it differs from the type's own default:
	// the default was not a choice the author made.
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''),
		       a.attidentity, a.attgenerated,
		       CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
		            THEN COALESCE(co.collname, '') ELSE '' END,
		       COALESCE(d.description, '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
		LEFT JOIN pg_collation co ON co.oid = a.attcollation
		LEFT JOIN pg_description d
		  ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = a.attnum
		WHERE c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND `+notSystem+`
		ORDER BY n.nspname, c.relname, a.attnum`)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k tableKey
		var col schema.Column
		var identity, generated string
		if err := rows.Scan(&k.ns, &k.table, &col.Name, &col.Type, &col.Nullable,
			&col.Default, &identity, &generated, &col.Collation, &col.Comment); err != nil {
			return fmt.Errorf("scan column: %w", err)
		}
		switch identity {
		case "a":
			col.Identity = "ALWAYS"
		case "d":
			col.Identity = "BY DEFAULT"
		}
		// A generated column's expression lives in the default slot; move it so
		// the two are never confused by the diff engine.
		if generated == "s" {
			col.Generated, col.Default = col.Default, ""
		}
		if t, ok := tables[k]; ok {
			t.Columns = append(t.Columns, col)
		}
	}
	return rows.Err()
}

// fkAction maps pg_constraint's action codes to SQL keywords.
func fkAction(code string) string {
	switch code {
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	default: // "a" — NO ACTION, the default; normalization drops it.
		return ""
	}
}

func readConstraints(ctx context.Context, q Querier, tables map[tableKey]*schema.Table) error {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, con.conname, con.contype,
		       pg_get_constraintdef(con.oid),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                 FROM unnest(con.conkey) WITH ORDINALITY k(attnum, ord)
		                 JOIN pg_attribute a
		                   ON a.attrelid = con.conrelid AND a.attnum = k.attnum), '{}'),
		       COALESCE(rn.nspname, ''), COALESCE(rc.relname, ''),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                 FROM unnest(con.confkey) WITH ORDINALITY k(attnum, ord)
		                 JOIN pg_attribute a
		                   ON a.attrelid = con.confrelid AND a.attnum = k.attnum), '{}'),
		       con.confdeltype, con.confupdtype,
		       con.condeferrable, con.condeferred,
		       COALESCE(pg_get_expr(con.conbin, con.conrelid), '')
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_class rc ON rc.oid = con.confrelid
		LEFT JOIN pg_namespace rn ON rn.oid = rc.relnamespace
		WHERE con.contype IN ('p', 'f', 'u', 'c', 'x')
		  AND `+notSystem+` AND `+extensionOwned("pg_constraint", "con.oid")+`
		ORDER BY n.nspname, c.relname, con.conname`)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	kinds := map[string]schema.ConstraintType{
		"p": schema.PrimaryKey, "f": schema.ForeignKey,
		"u": schema.Unique, "c": schema.Check, "x": schema.Exclusion,
	}

	for rows.Next() {
		var k tableKey
		var con schema.Constraint
		var kind, onDelete, onUpdate string
		if err := rows.Scan(&k.ns, &k.table, &con.Name, &kind, &con.Definition,
			&con.Columns, &con.RefNamespace, &con.RefTable, &con.RefColumns,
			&onDelete, &onUpdate, &con.Deferrable, &con.InitiallyDeferred,
			&con.Expression); err != nil {
			return fmt.Errorf("scan constraint: %w", err)
		}
		con.Type = kinds[kind]
		if con.Type == schema.ForeignKey {
			con.OnDelete, con.OnUpdate = fkAction(onDelete), fkAction(onUpdate)
		} else {
			// These columns are meaningless for non-foreign-keys and must not
			// reach the model, where they would perturb the fingerprint.
			con.RefNamespace, con.RefTable, con.RefColumns = "", "", nil
		}
		if con.Type != schema.Check && con.Type != schema.Exclusion {
			con.Expression = ""
		}
		if t, ok := tables[k]; ok {
			t.Constraints = append(t.Constraints, con)
		}
	}
	return rows.Err()
}

func readIndexes(ctx context.Context, q Querier, tables map[tableKey]*schema.Table) error {
	// pg_get_indexdef(oid, n, true) renders the nth key, which handles
	// expression indexes that have no column name to report.
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, ic.relname, i.indisunique, am.amname,
		       COALESCE((SELECT array_agg(pg_get_indexdef(i.indexrelid, k, true) ORDER BY k)
		                 FROM generate_series(1, i.indnkeyatts) k), '{}'),
		       COALESCE((SELECT array_agg(pg_get_indexdef(i.indexrelid, k, true) ORDER BY k)
		                 FROM generate_series(i.indnkeyatts + 1, i.indnatts) k), '{}'),
		       COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
		       pg_get_indexdef(i.indexrelid),
		       COALESCE(d.description, '')
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_am am ON am.oid = ic.relam
		LEFT JOIN pg_description d
		  ON d.objoid = ic.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
		WHERE NOT c.relispartition
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint con
		                  WHERE con.conindid = i.indexrelid
		                    AND con.contype IN ('p', 'u', 'x'))
		  AND `+notSystem+` AND `+extensionOwned("pg_class", "ic.oid")+`
		ORDER BY n.nspname, c.relname, ic.relname`)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k tableKey
		var idx schema.Index
		if err := rows.Scan(&k.ns, &k.table, &idx.Name, &idx.Unique, &idx.Method,
			&idx.Columns, &idx.Include, &idx.Predicate, &idx.Definition,
			&idx.Comment); err != nil {
			return fmt.Errorf("scan index: %w", err)
		}
		if t, ok := tables[k]; ok {
			t.Indexes = append(t.Indexes, idx)
		}
	}
	return rows.Err()
}

func readSequences(ctx context.Context, q Querier, namespaces map[string]schema.Namespace) error {
	// deptype 'a' and 'i' mark a sequence owned by a serial or identity column.
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, format_type(s.seqtypid, NULL),
		       s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
		       COALESCE(oc.relname, ''), COALESCE(oa.attname, ''),
		       COALESCE(d.description, '')
		FROM pg_sequence s
		JOIN pg_class c ON c.oid = s.seqrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_depend dep
		  ON dep.classid = 'pg_class'::regclass AND dep.objid = c.oid
		 AND dep.refclassid = 'pg_class'::regclass AND dep.deptype IN ('a', 'i')
		LEFT JOIN pg_class oc ON oc.oid = dep.refobjid
		LEFT JOIN pg_attribute oa
		  ON oa.attrelid = dep.refobjid AND oa.attnum = dep.refobjsubid
		LEFT JOIN pg_description d
		  ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
		WHERE `+notSystem+` AND `+extensionOwned("pg_class", "c.oid")+`
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return fmt.Errorf("read sequences: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var nsName string
		var seq schema.Sequence
		if err := rows.Scan(&nsName, &seq.Name, &seq.Type, &seq.Start, &seq.Increment,
			&seq.Min, &seq.Max, &seq.Cache, &seq.Cycle,
			&seq.OwnedByTable, &seq.OwnedByColumn, &seq.Comment); err != nil {
			return fmt.Errorf("scan sequence: %w", err)
		}
		if ns, ok := namespaces[nsName]; ok {
			ns.Sequences = append(ns.Sequences, seq)
			namespaces[nsName] = ns
		}
	}
	return rows.Err()
}

func readEnums(ctx context.Context, q Querier, namespaces map[string]schema.Namespace) error {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, t.typname,
		       array_agg(e.enumlabel ORDER BY e.enumsortorder),
		       COALESCE(max(d.description), '')
		FROM pg_type t
		JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		LEFT JOIN pg_description d
		  ON d.objoid = t.oid AND d.classoid = 'pg_type'::regclass
		WHERE `+notSystem+` AND `+extensionOwned("pg_type", "t.oid")+`
		GROUP BY n.nspname, t.typname
		ORDER BY n.nspname, t.typname`)
	if err != nil {
		return fmt.Errorf("read enums: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var nsName string
		var e schema.Enum
		if err := rows.Scan(&nsName, &e.Name, &e.Labels, &e.Comment); err != nil {
			return fmt.Errorf("scan enum: %w", err)
		}
		if ns, ok := namespaces[nsName]; ok {
			ns.Enums = append(ns.Enums, e)
			namespaces[nsName] = ns
		}
	}
	return rows.Err()
}
