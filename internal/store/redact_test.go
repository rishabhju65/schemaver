package store

import (
	"strings"
	"testing"
)

// TestRedactStripsCredentials guards the one boundary in the activity log that
// cannot be fixed after the fact.
//
// The log is kept indefinitely and never updated, so a password written into it
// is there permanently. Both forms below arrive inside error text that nobody
// composed deliberately — a driver quoting back the connection string it failed
// on — which is exactly why stripping them cannot be left to the caller who
// happens to be passing the error along.
func TestRedactStripsCredentials(t *testing.T) {
	for _, c := range []struct{ name, in, mustNotContain string }{
		{
			name:           "connection string in a failed dial",
			in:             `failed to connect to postgres://schemaver:s3cr3t@db.internal:5432/shop`,
			mustNotContain: "s3cr3t",
		},
		{
			name:           "postgresql scheme",
			in:             `dial error: postgresql://admin:hunter2@10.0.0.4:5432/x?sslmode=require`,
			mustNotContain: "hunter2",
		},
		{
			name:           "keyword form",
			in:             `bad config: host=db user=schemaver password=letmein sslmode=require`,
			mustNotContain: "letmein",
		},
		{
			name:           "quoted keyword form",
			in:             `host=db password='p@ss word' sslmode=require`,
			mustNotContain: "p@ss word",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := redact(c.in)
			if strings.Contains(got, c.mustNotContain) {
				t.Fatalf("the credential survived redaction:\n  in:  %s\n  out: %s", c.in, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Errorf("nothing was marked as redacted: %s", got)
			}
		})
	}

	// Text with no credential in it must come through untouched, or the log
	// starts lying about statements that merely mention the word.
	for _, in := range []string{
		"ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)",
		"ALTER TABLE public.users ADD COLUMN password_hash text;",
		"applied 3 statement(s)",
	} {
		if got := redact(in); got != in {
			t.Errorf("redaction altered innocent text:\n  in:  %s\n  out: %s", in, got)
		}
	}
}

// TestScrubLiteralsKeepsShapeAndDropsValues covers the one thing in the
// activity log written by somebody who never agreed to be recorded: the query
// text of a session that happened to be blocking a migration.
func TestScrubLiteralsKeepsShapeAndDropsValues(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{
			`SELECT * FROM orders WHERE email = 'ada@example.com'`,
			`SELECT * FROM orders WHERE email = '?'`,
		},
		{
			// An escaped quote must not end the string early, or everything
			// after it leaks through as if it were SQL.
			`UPDATE users SET name = 'O''Hara' WHERE id = 42`,
			`UPDATE users SET name = '?' WHERE id = ?`,
		},
		{
			// Identifiers are what a reader needs; they survive.
			`SELECT "Order_2" FROM "Public"."Orders" WHERE total > 99.95`,
			`SELECT "Order_2" FROM "Public"."Orders" WHERE total > ?`,
		},
		{
			// A digit inside a name is part of the name, not a value.
			`SELECT column_1, addr2 FROM t1 LIMIT 100`,
			`SELECT column_1, addr2 FROM t1 LIMIT ?`,
		},
		{
			// A function body can contain anything, so it goes wholesale.
			`CREATE FUNCTION f() RETURNS int AS $$ SELECT 'secret' $$ LANGUAGE sql`,
			`CREATE FUNCTION f() RETURNS int AS $$?$$ LANGUAGE sql`,
		},
		{
			// The shape a person actually reads off a blocked migration.
			`BEGIN; SELECT count(*) FROM orders WHERE created_at > '2026-01-01'`,
			`BEGIN; SELECT count(*) FROM orders WHERE created_at > '?'`,
		},
	} {
		if got := ScrubLiterals(c.in); got != c.want {
			t.Errorf("scrubbing\n  in:   %s\n  got:  %s\n  want: %s", c.in, got, c.want)
		}
	}

	// The property that matters more than any single case: no run of letters
	// from inside a quoted string may survive.
	secret := `SELECT * FROM people WHERE surname = 'Lovelace' AND card = '4111111111111111'`
	got := ScrubLiterals(secret)
	for _, leaked := range []string{"Lovelace", "4111111111111111"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived scrubbing: %s", leaked, got)
		}
	}
}
