package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// planDigest identifies the statements a decision was made about.
//
// Covers the migration and its revert together, because they are reviewed and
// approved as one: somebody who adjusts the way back has changed what the
// approval was for just as surely as somebody who adjusts the way forward.
//
// The ordinal is digested alongside the text so that reordering two statements
// changes the digest even when every statement is individually unchanged.
// Ordering is most of what a migration is — a drop before the index that
// depends on it is a different plan from the reverse — and a digest that
// ignored it would call two different plans the same.
func planDigest(steps []Step) string {
	h := sha256.New()
	for _, st := range steps {
		fmt.Fprintf(h, "%d:%s\n", st.Ordinal, st.SQL)
	}
	// A trailing separator that once divided the forward statements from the
	// way back. Kept now that there is no way back, because removing it would
	// change the digest of every plan that never had one — withdrawing
	// approvals of statements nobody has touched, which is the opposite of what
	// the digest is for.
	io.WriteString(h, "--\n")
	return hex.EncodeToString(h.Sum(nil))
}

// SplitStatements breaks authored SQL into statements on semicolons.
//
// Quoting is respected, because a semicolon inside a string literal or a
// dollar-quoted function body is data rather than a boundary — splitting there
// would cut a statement in half and report a syntax error against SQL the
// author wrote correctly.
//
// Deliberately not a parser. It knows where a statement ends and nothing about
// what is in it; the engine is the authority on whether any of it is valid, and
// the shadow is where that gets established.
func SplitStatements(sql string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" && !onlyComments(s) {
			out = append(out, s+";")
		}
		cur.Reset()
	}

	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case c == '\'' || c == '"':
			// A quoted run, where a doubled quote is an escape rather than the
			// end of one.
			quote := c
			cur.WriteByte(c)
			i++
			for i < len(sql) {
				cur.WriteByte(sql[i])
				if sql[i] == quote {
					if i+1 < len(sql) && sql[i+1] == quote {
						cur.WriteByte(sql[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}

		case c == '$':
			if tag, end, ok := dollarTag(sql, i); ok {
				if close := strings.Index(sql[end:], tag); close >= 0 {
					stop := end + close + len(tag)
					cur.WriteString(sql[i:stop])
					i = stop
					continue
				}
			}
			cur.WriteByte(c)
			i++

		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			// A line comment runs to the newline and can contain anything.
			for i < len(sql) && sql[i] != '\n' {
				cur.WriteByte(sql[i])
				i++
			}

		case c == ';':
			flush()
			i++

		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

// onlyComments reports SQL that would execute nothing.
func onlyComments(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}
