package introspect

import (
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// TestAnInvalidIndexIsNotTheIndexItWasMeantToBe covers a failure that used to
// report itself as a success.
//
// A CREATE INDEX CONCURRENTLY that fails leaves the index in the catalogue
// marked invalid: it has the right name, pg_get_indexdef renders it identically
// to a working one, and no query will ever use it. Nothing read indisvalid, so
// the model could not tell the two apart — which meant a three-hour build that
// failed on a large table read back as exactly the schema it had been trying to
// reach. The migration looked done, drift saw nothing, and the planner ignored
// the index forever.
func TestAnInvalidIndexIsNotTheIndexItWasMeantToBe(t *testing.T) {
	conn, ctx := connect(t)

	// A unique index over duplicated values: the build starts, scans, and fails.
	// Run outside the scratch-schema helper because CONCURRENTLY cannot run in
	// a transaction block and needs its own error tolerated.
	apply(t, conn, ctx, "invalid_idx_probe", `
		CREATE TABLE t (id bigint, code text);
		INSERT INTO t VALUES (1, 'x'), (2, 'x');
	`)
	if _, err := conn.Exec(ctx,
		"CREATE UNIQUE INDEX CONCURRENTLY t_code_idx ON t (code)"); err == nil {
		t.Fatal("the build was supposed to fail on the duplicate")
	}

	// PostgreSQL must actually have left one behind, or this proves nothing.
	var valid bool
	if err := conn.QueryRow(ctx, `
		SELECT i.indisvalid FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 't_code_idx'`).Scan(&valid); err != nil {
		t.Fatalf("the failed build left no index at all: %v", err)
	}
	if valid {
		t.Fatal("the index came out valid; this test proves nothing")
	}

	got, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	tbl := findTable(got, "invalid_idx_probe", "t")
	if tbl == nil {
		t.Fatal("table not found")
	}
	var found *schema.Index
	for i := range tbl.Indexes {
		if tbl.Indexes[i].Name == "t_code_idx" {
			found = &tbl.Indexes[i]
		}
	}
	if found == nil {
		t.Fatal("the index is not reported at all; it exists and occupies its name")
	}
	if !found.Invalid {
		t.Error("an index the engine will never use is reported as though it works")
	}

	// And it must change the fingerprint, or the database still reads as being
	// at the schema the build was aiming for.
	broken, err := schema.Fingerprint(got)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	repaired := cloneWithValidIndex(got)
	whole, err := schema.Fingerprint(repaired)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if broken == whole {
		t.Errorf("a database with an unusable index fingerprints the same as one "+
			"with a working index (%s); a failed build would read back as success",
			broken.Short())
	}
}

// cloneWithValidIndex returns the schema as it would be if the build had worked.
func cloneWithValidIndex(s *schema.Schema) *schema.Schema {
	out := &schema.Schema{}
	for _, ns := range s.Namespaces {
		copied := ns
		copied.Tables = nil
		for _, t := range ns.Tables {
			ct := t
			ct.Indexes = nil
			for _, i := range t.Indexes {
				ci := i
				ci.Invalid = false
				ct.Indexes = append(ct.Indexes, ci)
			}
			copied.Tables = append(copied.Tables, ct)
		}
		out.Namespaces = append(out.Namespaces, copied)
	}
	return out
}

// TestAValidIndexSerializesAsItAlwaysDid is why this change needed no
// re-baselining.
//
// Every stored fingerprint in every deployment was taken before the model knew
// about index validity. `omitempty` is what keeps them all true: a valid index
// writes no field at all, so the bytes hashed are the ones that were hashed
// before.
func TestAValidIndexSerializesAsItAlwaysDid(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "valid_idx_probe", `
		CREATE TABLE t (id bigint, code text);
		CREATE UNIQUE INDEX t_code_idx ON t (code);
	`)

	full, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// Narrowed to this test's own namespace. Schema reads the whole database,
	// and `go test ./...` runs packages against a shared one — a concurrent
	// index build in another package is briefly invalid, and judging it here
	// would fail a test that has nothing to do with it.
	got := only(full, "valid_idx_probe")
	if len(got.Namespaces) == 0 {
		t.Fatal("the probe namespace is missing from the introspected schema")
	}
	canonical, err := schema.Canonical(got)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for _, ns := range got.Namespaces {
		for _, tbl := range ns.Tables {
			for _, idx := range tbl.Indexes {
				if idx.Invalid {
					t.Fatalf("index %s came out invalid; this test proves nothing", idx.Name)
				}
			}
		}
	}
	if containsInvalidKey(canonical) {
		t.Error("a valid index writes an \"invalid\" key, so every fingerprint " +
			"taken before this field existed would move")
	}
}

func containsInvalidKey(canonical []byte) bool {
	const key = `"invalid"`
	for i := 0; i+len(key) <= len(canonical); i++ {
		if string(canonical[i:i+len(key)]) == key {
			return true
		}
	}
	return false
}

// only returns s carrying just the named namespace.
func only(s *schema.Schema, name string) *schema.Schema {
	out := *s
	out.Namespaces = nil
	for _, ns := range s.Namespaces {
		if ns.Name == name {
			out.Namespaces = append(out.Namespaces, ns)
		}
	}
	return &out
}
