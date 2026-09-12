package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// RevertStep is one statement of the generated way back.
type RevertStep struct {
	Ordinal       int
	SQL           string
	ChangeID      string
	Transactional bool
	Note          string
	// StructureOnly reports a statement that puts the shape back and cannot put
	// the contents back.
	StructureOnly bool
}

// buildRevert derives the migration that undoes this one.
//
// The same engine, given the same two schemas in the other order. A revert is
// not a special kind of artifact and nothing here knows how to invert a
// statement: it is the diff from where the migration ends to where it began,
// which is a question the diff engine already answers.
//
// What this adds is which of those statements are honest. The revert of
// `DROP COLUMN legacy_status` is `ADD COLUMN legacy_status text` — a perfectly
// good statement that restores an empty column. A reviewer reading a list of
// revert statements without that marked would read it as an undo, and the
// moment to discover it is not one is before the forward migration runs.
func buildRevert(forward diff.Result, from, to *schema.Schema) ([]RevertStep, []diff.Change) {
	// Objects the forward migration destroys. Their contents cannot come back,
	// whatever DDL is generated for them.
	lost := map[string]bool{}
	for _, c := range forward.Changes {
		if c.Class == diff.Destructive {
			lost[c.Qualified()] = true
		}
	}

	back := diff.Compute(to, from)
	statements := render.Statements(back.Changes, to, from)

	byID := make(map[string]diff.Change, len(back.Changes))
	for _, c := range back.Changes {
		byID[c.ID] = c
	}

	steps := make([]RevertStep, 0, len(statements))
	for i, st := range statements {
		step := RevertStep{
			Ordinal: i + 1, SQL: st.SQL, ChangeID: st.ChangeID,
			Transactional: st.Transactional, Note: st.Note,
		}
		// Matched on the object rather than the change id, because the ids
		// differ by design: the forward change is drop_column:x and its
		// counterpart here is add_column:x. What they share is the thing they
		// are about.
		if c, ok := byID[st.ChangeID]; ok && lost[c.Qualified()] {
			step.StructureOnly = true
			step.Note = joinNote(step.Note,
				"restores the shape only — the contents this column held are "+
					"discarded by the forward migration and no generated DDL brings "+
					"them back")
		}
		steps = append(steps, step)
	}
	return steps, back.Changes
}

func joinNote(existing, added string) string {
	if existing == "" {
		return added
	}
	return existing + "; " + added
}

// RevertSteps reads the generated way back for a migration.
func (s *Scope) RevertSteps(ctx context.Context, migrationID int64) ([]RevertStep, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT rs.ordinal, rs.sql, rs.change_id, rs.transactional,
		       COALESCE(rs.note, ''), rs.structure_only
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
		if err := rows.Scan(&st.Ordinal, &st.SQL, &st.ChangeID,
			&st.Transactional, &st.Note, &st.StructureOnly); err != nil {
			return nil, fmt.Errorf("scan revert step: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// planDigest identifies the statements a decision was made about.
//
// Covers the migration and its revert together, because they are reviewed and
// approved as one: an administrator who adjusts the way back has changed what
// the approval was for just as surely as one who adjusts the way forward.
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
