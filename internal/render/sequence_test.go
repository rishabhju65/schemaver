package render_test

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// serialTable is a table whose primary key was declared `bigserial`, as the
// engine reports it back: a plain bigint whose default calls a sequence, and a
// separate sequence owned by that column.
func serialTable() *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{{Name: "coupon", Columns: []schema.Column{
			{Name: "id", Type: "bigint", Default: "nextval('public.coupon_id_seq'::regclass)"},
			{Name: "code", Type: "text"},
		}}},
		Sequences: []schema.Sequence{{
			Name: "coupon_id_seq", Type: "bigint", Start: 1, Increment: 1,
			Min: 1, Max: 9223372036854775807, Cache: 1,
			OwnedByTable: "coupon", OwnedByColumn: "id",
		}},
	}}}
}

// identityTable is the same shape declared GENERATED ALWAYS AS IDENTITY, which
// the engine also backs with an owned sequence — but one the column declaration
// creates by itself.
func identityTable() *schema.Schema {
	return &schema.Schema{Namespaces: []schema.Namespace{{
		Name: "public",
		Tables: []schema.Table{{Name: "invoice", Columns: []schema.Column{
			{Name: "id", Type: "bigint", Identity: "ALWAYS"},
		}}},
		Sequences: []schema.Sequence{{
			Name: "invoice_id_seq", Type: "bigint", Start: 1, Increment: 1,
			Min: 1, Max: 9223372036854775807, Cache: 1,
			OwnedByTable: "invoice", OwnedByColumn: "id",
		}},
	}}}
}

// TestASerialColumnsSequenceIsCreated covers DDL that could not run.
//
// Owned sequences were skipped on the assumption that the column owning them
// creates them. That is true of `bigserial` as somebody writes it and false of
// what it becomes: the engine reports the column as `bigint DEFAULT
// nextval(...)`, and a default creates nothing. Since this is how every shadow
// rebuilds a schema, no database with a serial column could be rehearsed
// against, proven, branched from, or have written statements derived for it.
func TestASerialColumnsSequenceIsCreated(t *testing.T) {
	ddl := render.Schema(serialTable())
	seq := strings.Index(ddl, "CREATE SEQUENCE public.coupon_id_seq")
	table := strings.Index(ddl, "CREATE TABLE public.coupon")
	owned := strings.Index(ddl, "ALTER SEQUENCE public.coupon_id_seq OWNED BY public.coupon.id")

	if seq < 0 {
		t.Fatalf("the sequence the column defaults to is never created:\n%s", ddl)
	}
	if seq > table {
		t.Error("the sequence is created after the table that defaults to it")
	}
	if owned < 0 {
		t.Errorf("the sequence is never given back to its column, so dropping "+
			"the column would leave it behind:\n%s", ddl)
	}
	if owned < table {
		t.Error("ownership is attached before the table exists")
	}
}

// TestAnIdentityColumnsSequenceIsLeftAlone is the other half. Both kinds of
// column own a sequence and the difference is invisible in the sequence itself
// — creating an identity column's would fail as a duplicate.
func TestAnIdentityColumnsSequenceIsLeftAlone(t *testing.T) {
	ddl := render.Schema(identityTable())
	if strings.Contains(ddl, "CREATE SEQUENCE") {
		t.Errorf("an identity column creates its own sequence; this would be "+
			"a duplicate:\n%s", ddl)
	}
	if strings.Contains(ddl, "OWNED BY") {
		t.Errorf("an identity column already owns its sequence:\n%s", ddl)
	}
	if !strings.Contains(ddl, "GENERATED ALWAYS AS IDENTITY") {
		t.Errorf("the column lost its identity declaration:\n%s", ddl)
	}
}

// TestCreatingATableBringsItsSequence covers the same gap in the statements a
// migration is made of. Nothing else in a change list would create it: an owned
// sequence is deliberately not diffed as an object of its own.
func TestCreatingATableBringsItsSequence(t *testing.T) {
	to := serialTable()
	from := &schema.Schema{Namespaces: []schema.Namespace{{Name: "public"}}}

	statements := render.Statements(diff.Compute(from, to).Changes, from, to)
	var sql []string
	for _, s := range statements {
		sql = append(sql, s.SQL)
	}
	joined := strings.Join(sql, "\n")

	if !strings.Contains(joined, "CREATE SEQUENCE public.coupon_id_seq") {
		t.Fatalf("creating the table does not create the sequence it needs:\n%s", joined)
	}
	if !strings.Contains(joined, "ALTER SEQUENCE public.coupon_id_seq OWNED BY") {
		t.Errorf("the sequence is never given to its column:\n%s", joined)
	}

	var seqAt, tableAt = -1, -1
	for i, s := range sql {
		if strings.HasPrefix(s, "CREATE SEQUENCE") {
			seqAt = i
		}
		if strings.HasPrefix(s, "CREATE TABLE") {
			tableAt = i
		}
	}
	if seqAt < 0 || tableAt < 0 || seqAt > tableAt {
		t.Errorf("the sequence must be created before the table, got order %v", sql)
	}

	// Every statement still belongs to the change it serves, or the review page
	// cannot attribute it and a discussion thread cannot anchor to it.
	for _, s := range statements {
		if s.ChangeID == "" {
			t.Errorf("statement %q carries no change id", s.SQL)
		}
	}
}
