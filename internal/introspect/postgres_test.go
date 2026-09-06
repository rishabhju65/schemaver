package introspect

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// connect returns a connection to the test database, skipping the test when
// none is configured. Introspection cannot be meaningfully faked — the whole
// point is what a real engine reports — so these are integration tests or
// nothing.
func connect(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	url := os.Getenv("SCHEMAVER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_TEST_DATABASE_URL to run introspection tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, ctx
}

// apply runs ddl in a scratch namespace that is dropped when the test ends, so
// tests neither collide nor leak.
func apply(t *testing.T, conn *pgx.Conn, ctx context.Context, ns, ddl string) {
	t.Helper()
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE"); err != nil {
		t.Fatalf("drop scratch schema: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+ns); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("apply ddl: %v", err)
	}
}

func findTable(s *schema.Schema, ns, name string) *schema.Table {
	for i := range s.Namespaces {
		if s.Namespaces[i].Name != ns {
			continue
		}
		for j := range s.Namespaces[i].Tables {
			if s.Namespaces[i].Tables[j].Name == name {
				return &s.Namespaces[i].Tables[j]
			}
		}
	}
	return nil
}

// TestIntrospectIsDeterministic is the property everything rests on: reading the
// same logical schema twice must produce the same fingerprint.
//
// The two reads are of a schema built, dropped and rebuilt from identical DDL,
// so every engine-assigned artifact — object identifiers, physical ordering — is
// different between them and must not reach the model.
func TestIntrospectIsDeterministic(t *testing.T) {
	conn, ctx := connect(t)

	const ddl = `
		CREATE TYPE sv_status AS ENUM ('pending', 'paid', 'shipped');
		CREATE TABLE customers (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email text NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_id bigint NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			status sv_status NOT NULL DEFAULT 'pending',
			total numeric(12,2) CHECK (total >= 0),
			created_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX orders_customer_idx ON orders (customer_id, created_at DESC);
		CREATE INDEX orders_open_idx ON orders (created_at) WHERE status <> 'shipped';`

	apply(t, conn, ctx, "sv_test_det", ddl)
	first, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	fpA, err := schema.Fingerprint(first)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	// Rebuild the identical schema from scratch under the same name.
	apply(t, conn, ctx, "sv_test_det", ddl)
	second, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	fpB, err := schema.Fingerprint(second)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	if fpA != fpB {
		t.Errorf("rebuilding an identical schema changed its fingerprint:\n  %s\n  %s", fpA, fpB)
	}
	t.Logf("schema fingerprint: %s", fpA.Short())
}

// TestIntrospectIgnoresSearchPath is a regression test.
//
// format_type renders a type as "st" or "myschema.st" depending on whether its
// schema is in the caller's search path. Left unhandled, the same database read
// by two connections produces two different fingerprints — the exact failure the
// canonical model exists to prevent. Introspection therefore reads with an empty
// search_path so type names are always fully qualified.
func TestIntrospectIgnoresSearchPath(t *testing.T) {
	conn, ctx := connect(t)

	apply(t, conn, ctx, "sv_test_path", `
		CREATE TYPE flavour AS ENUM ('sweet', 'salt');
		CREATE TABLE snack (id bigint PRIMARY KEY, kind flavour NOT NULL);`)

	read := func(searchPath string) schema.Version {
		t.Helper()
		if _, err := conn.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
			t.Fatalf("set search_path: %v", err)
		}
		s, err := Schema(ctx, conn)
		if err != nil {
			t.Fatalf("introspect with search_path %s: %v", searchPath, err)
		}
		v, err := schema.Fingerprint(s)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return v
	}

	inPath := read("sv_test_path")
	outOfPath := read("public")
	if inPath != outOfPath {
		t.Errorf("fingerprint depends on the caller's search_path:\n  in path     %s\n  out of path %s",
			inPath, outOfPath)
	}
}

// TestIntrospectStructure checks the model captures what the diff engine will
// need, rather than merely producing a stable hash of something wrong.
func TestIntrospectStructure(t *testing.T) {
	conn, ctx := connect(t)
	apply(t, conn, ctx, "sv_test_s", `
		CREATE TABLE parent (id bigint PRIMARY KEY);
		CREATE TABLE child (
			id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			parent_id bigint NOT NULL REFERENCES parent(id) ON DELETE SET NULL,
			label text NOT NULL,
			label_upper text GENERATED ALWAYS AS (upper(label)) STORED,
			CONSTRAINT child_label_len CHECK (length(label) > 0)
		);
		CREATE UNIQUE INDEX child_label_idx ON child (label) INCLUDE (parent_id);`)

	s, err := Schema(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	child := findTable(s, "sv_test_s", "child")
	if child == nil {
		t.Fatal("child table missing")
	}

	cols := map[string]schema.Column{}
	for _, c := range child.Columns {
		cols[c.Name] = c
	}
	if got := cols["id"].Identity; got != "BY DEFAULT" {
		t.Errorf("identity: got %q, want %q", got, "BY DEFAULT")
	}
	if cols["label_upper"].Generated == "" {
		t.Error("generated column expression was not captured")
	}
	if cols["label_upper"].Default != "" {
		t.Error("generation expression leaked into Default; diff would treat it as a default")
	}

	var fk, check *schema.Constraint
	for i := range child.Constraints {
		switch child.Constraints[i].Type {
		case schema.ForeignKey:
			fk = &child.Constraints[i]
		case schema.Check:
			check = &child.Constraints[i]
		}
	}
	if fk == nil {
		t.Fatal("foreign key not captured")
	}
	if fk.RefTable != "parent" || fk.OnDelete != "SET NULL" {
		t.Errorf("foreign key: got ref=%s on_delete=%q", fk.RefTable, fk.OnDelete)
	}
	if check == nil || check.Expression == "" {
		t.Error("check constraint expression not captured")
	}

	// The primary key's backing index must not also appear as an index.
	for _, idx := range child.Indexes {
		if idx.Name == "child_pkey" {
			t.Error("constraint-backed index leaked into Indexes; it would diff twice")
		}
	}
	var labelIdx *schema.Index
	for i := range child.Indexes {
		if child.Indexes[i].Name == "child_label_idx" {
			labelIdx = &child.Indexes[i]
		}
	}
	if labelIdx == nil {
		t.Fatal("child_label_idx missing")
	}
	if !labelIdx.Unique || len(labelIdx.Include) != 1 || labelIdx.Include[0] != "parent_id" {
		t.Errorf("index: unique=%v include=%v", labelIdx.Unique, labelIdx.Include)
	}
}
