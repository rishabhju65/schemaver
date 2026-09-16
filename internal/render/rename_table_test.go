package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

const renameNS = "rename_rows"

func ordersAt(name string, columns ...schema.Column) *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name:   renameNS,
		Tables: []schema.Table{{Name: name, Columns: columns}},
	}}}
}

func col(name, typ string) schema.Column {
	return schema.Column{Name: name, Type: typ, Nullable: true}
}

// TestARenamedTableArrivesWithItsRows is the claim the whole question exists to
// protect, checked against a real database rather than against the change list.
//
// Everything upstream can agree that this is a rename and still lose the data
// if the statement that comes out is a DROP and a CREATE. So this one counts
// rows.
func TestARenamedTableArrivesWithItsRows(t *testing.T) {
	url := os.Getenv("SCHEMAVER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SCHEMAVER_TEST_DATABASE_URL to run this test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered before the drop so it runs after it: a deferred close would
	// run first and the cleanup would execute against a closed connection.
	t.Cleanup(func() { conn.Close(context.Background()) })

	if _, err := conn.Exec(ctx, `
		DROP SCHEMA IF EXISTS `+renameNS+` CASCADE;
		CREATE SCHEMA `+renameNS+`;
		CREATE TABLE `+renameNS+`.orders (id bigint, note text);
		INSERT INTO `+renameNS+`.orders
		     SELECT g, 'row ' || g FROM generate_series(1, 1000) g`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+renameNS+` CASCADE`)
	})

	from := ordersAt("orders", col("id", "bigint"), col("note", "text"))
	to := ordersAt("purchase", col("id", "bigint"), col("note", "text"))

	result := diff.ComputeWith(from, to, []diff.Rename{
		{Namespace: renameNS, From: "orders", To: "purchase"},
	})
	statements := render.Statements(result.Changes, from, to)
	if len(statements) == 0 {
		t.Fatal("nothing to run")
	}
	for _, s := range statements {
		if _, err := conn.Exec(ctx, s.SQL); err != nil {
			t.Fatalf("running %q: %v", s.SQL, err)
		}
	}

	var rows int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM `+renameNS+`.purchase`).Scan(&rows); err != nil {
		t.Fatalf("the renamed table cannot be read: %v", err)
	}
	if rows != 1000 {
		t.Errorf("the renamed table holds %d rows, not the 1000 it had; the "+
			"rename discarded data it was chosen in order to keep", rows)
	}

	var old int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = 'orders'`, renameNS).Scan(&old); err != nil {
		t.Fatalf("check the old name: %v", err)
	}
	if old != 0 {
		t.Error("the old table is still there, so this copied rather than renamed")
	}
}

// TestDroppingATableStillDropsIt is the other half. Without the confirmation
// the drop is what was meant, and it has to still happen.
func TestDroppingATableStillDropsIt(t *testing.T) {
	from := ordersAt("orders", col("id", "bigint"), col("note", "text"))
	to := ordersAt("purchase", col("id", "bigint"), col("note", "text"))

	statements := render.Statements(diff.Compute(from, to).Changes, from, to)
	var sawDrop, sawCreate, sawRename bool
	for _, s := range statements {
		switch {
		case strings.Contains(s.SQL, "DROP TABLE"):
			sawDrop = true
		case strings.Contains(s.SQL, "CREATE TABLE"):
			sawCreate = true
		case strings.Contains(s.SQL, "RENAME TO"):
			sawRename = true
		}
	}
	if sawRename {
		t.Error("renamed without anybody confirming it")
	}
	if !sawDrop || !sawCreate {
		t.Errorf("unconfirmed, this is a drop and a create: drop=%v create=%v",
			sawDrop, sawCreate)
	}
}
