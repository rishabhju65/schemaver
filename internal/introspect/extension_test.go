package introspect

import (
	"testing"
)

// TestAnExtensionsObjectsAreFilteredButItsSchemaIsNot pins behaviour that used
// to be attempted by a filter that could never match.
//
// The filter looked for a pg_depend row with classid pg_namespace and deptype
// 'e'. That row does not exist: PostgreSQL records the relationship as classid
// pg_extension, refclassid pg_namespace, deptype 'n', and records it the same
// way whether the extension created the schema or was installed into one that
// already existed. The distinction the filter wanted is not in the catalogue.
//
// PostgreSQL's own behaviour decides the right answer: a schema an extension
// creates survives DROP EXTENSION, so the engine does not treat it as owned.
// What belongs to the extension is its objects, and those are filtered.
func TestAnExtensionsObjectsAreFilteredButItsSchemaIsNot(t *testing.T) {
	conn, ctx := connect(t)

	apply(t, conn, ctx, "ext_probe", `SELECT 1`)
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS hstore WITH SCHEMA ext_probe"); err != nil {
		t.Skipf("hstore is not available here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, "DROP EXTENSION IF EXISTS hstore CASCADE")
	})

	// It genuinely put things there, or this proves nothing.
	var owned int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_depend d
		  JOIN pg_proc p ON p.oid = d.objid
		 WHERE d.classid = 'pg_proc'::regclass AND d.deptype = 'e'
		   AND p.pronamespace = 'ext_probe'::regnamespace`).Scan(&owned); err != nil {
		t.Fatalf("count extension objects: %v", err)
	}
	if owned == 0 {
		t.Skip("the extension put nothing in this schema")
	}

	got, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}

	var found bool
	for _, ns := range got.Namespaces {
		if ns.Name != "ext_probe" {
			continue
		}
		found = true
		// The extension's own objects are not the user's schema.
		if len(ns.Tables) != 0 {
			t.Errorf("%d extension-owned table(s) reached the model", len(ns.Tables))
		}
	}
	if !found {
		t.Error("the schema was filtered out for holding an extension; it is the " +
			"user's schema, and PostgreSQL keeps it when the extension goes")
	}
}
