package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// RevertStep is one statement of the way back, as somebody wrote it.
type RevertStep struct {
	Ordinal       int
	SQL           string
	Transactional bool
	Note          string
}

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
func planDigest(forward []Step, revert []RevertStep) string {
	h := sha256.New()
	for _, st := range forward {
		fmt.Fprintf(h, "%d:%s\n", st.Ordinal, st.SQL)
	}
	io.WriteString(h, "--\n")
	for _, st := range revert {
		fmt.Fprintf(h, "%d:%s\n", st.Ordinal, st.SQL)
	}
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

// RevertSteps reads the way back for a migration.
func (s *Scope) RevertSteps(ctx context.Context, migrationID int64) ([]RevertStep, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT rs.ordinal, rs.sql, rs.transactional, COALESCE(rs.note, '')
		  FROM schemaver.migration_revert_step rs
		  JOIN schemaver.migration m ON m.id = rs.migration_id
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		 WHERE rs.migration_id = $1 AND r.project_id = ANY($2)
		 ORDER BY rs.ordinal`, migrationID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("load revert steps: %w", err)
	}
	defer rows.Close()
	var out []RevertStep
	for rows.Next() {
		var st RevertStep
		if err := rows.Scan(&st.Ordinal, &st.SQL, &st.Transactional, &st.Note); err != nil {
			return nil, fmt.Errorf("scan revert step: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
