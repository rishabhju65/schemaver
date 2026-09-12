package store

import (
	"strings"
	"testing"
)

// TestSplitStatements covers where an authored script is cut into statements.
//
// This is the only place a person's SQL is interpreted before it reaches the
// engine, and getting it wrong is quiet: a semicolon inside a string or a
// function body would cut a statement in half, and the halves are often still
// syntactically plausible. The author would be shown an error about SQL they
// wrote correctly.
func TestSplitStatements(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain statements",
			in:   "DROP INDEX a;\nALTER TABLE t DROP COLUMN c;",
			want: []string{"DROP INDEX a;", "ALTER TABLE t DROP COLUMN c;"},
		},
		{
			name: "a trailing semicolon is optional",
			in:   "DROP INDEX a",
			want: []string{"DROP INDEX a;"},
		},
		{
			// The case that silently corrupts a script.
			name: "a semicolon inside a string is data",
			in:   "UPDATE t SET note = 'a; b' WHERE id = 1;",
			want: []string{"UPDATE t SET note = 'a; b' WHERE id = 1;"},
		},
		{
			name: "an escaped quote does not end the string",
			in:   "UPDATE t SET name = 'O''Hara; Ltd' WHERE id = 1;",
			want: []string{"UPDATE t SET name = 'O''Hara; Ltd' WHERE id = 1;"},
		},
		{
			name: "a function body is one statement however many semicolons it holds",
			in: "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; SELECT 1; END; $$ LANGUAGE plpgsql;\n" +
				"DROP TABLE t;",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $$ BEGIN; SELECT 1; END; $$ LANGUAGE plpgsql;",
				"DROP TABLE t;",
			},
		},
		{
			name: "a semicolon in a comment does not split",
			in:   "-- undo the index; it was added by mistake\nDROP INDEX a;",
			want: []string{"-- undo the index; it was added by mistake\nDROP INDEX a;"},
		},
		{
			name: "a quoted identifier survives",
			in:   `DROP TABLE "weird;name";`,
			want: []string{`DROP TABLE "weird;name";`},
		},
		{
			name: "blank and comment-only input yields nothing to run",
			in:   "\n\n-- nothing to do here\n\n",
			want: nil,
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := SplitStatements(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("split into %d statement(s), want %d:\n  %q", len(got), len(c.want), got)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != strings.TrimSpace(c.want[i]) {
					t.Errorf("statement %d:\n  got  %q\n  want %q", i+1, got[i], c.want[i])
				}
			}
		})
	}
}

// TestPlanDigestNoticesReordering guards the property that makes an approval
// specific to a plan rather than to a set of statements.
func TestPlanDigestNoticesReordering(t *testing.T) {
	a := []Step{{Ordinal: 1, SQL: "DROP INDEX i;"}, {Ordinal: 2, SQL: "DROP COLUMN c;"}}
	b := []Step{{Ordinal: 1, SQL: "DROP COLUMN c;"}, {Ordinal: 2, SQL: "DROP INDEX i;"}}
	if planDigest(a, nil) == planDigest(b, nil) {
		t.Error("two plans with the same statements in a different order digest " +
			"the same; dropping an index before or after the column it sits on " +
			"is not the same plan")
	}

	// And the revert counts, because it is approved alongside.
	withRevert := planDigest(a, []RevertStep{{Ordinal: 1, SQL: "CREATE INDEX i;"}})
	if withRevert == planDigest(a, nil) {
		t.Error("writing a revert did not change the digest; an approval given " +
			"before there was a way back would still count after one appeared")
	}
}
