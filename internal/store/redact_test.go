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
