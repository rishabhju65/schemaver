package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSQLLiteralsAreWellFormed reads this package's own source and checks every
// embedded query.
//
// It exists because of a real failure. Scoping every query to an owner was done
// partly by pattern-editing the SQL, which left several subqueries missing a
// closing parenthesis — and a malformed query compiles perfectly. Each one
// surfaced only when that particular code path first ran against a database,
// one at a time.
//
// Static checks catch the whole class at once, and cost nothing.
func TestSQLLiteralsAreWellFormed(t *testing.T) {
	query := regexp.MustCompile("(?s)`([^`]*(?:SELECT|INSERT|UPDATE|DELETE|CREATE|ALTER)[^`]*)`")

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}

	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range query.FindAllStringSubmatch(string(body), -1) {
			q := m[1]
			checked++
			oneLine := strings.Join(strings.Fields(q), " ")
			if len(oneLine) > 90 {
				oneLine = oneLine[:90] + "…"
			}

			if open, close := strings.Count(q, "("), strings.Count(q, ")"); open != close {
				t.Errorf("%s: %d open parentheses, %d close\n  %s", name, open, close, oneLine)
			}
			// A trailing comma before a clause keyword is the other thing
			// pattern-editing leaves behind.
			for _, broken := range []string{", FROM", ", WHERE", ",FROM", "( ,"} {
				if strings.Contains(oneLine, broken) {
					t.Errorf("%s: %q suggests a mangled column list\n  %s", name, broken, oneLine)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no queries found; the pattern no longer matches this package")
	}
	t.Logf("checked %d embedded queries", checked)
}
