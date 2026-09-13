package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rishabhju65/schemaver/internal/diff"
)

// RenameAnswer is one settled rename question, as the review page shows it.
type RenameAnswer struct {
	diff.Rename
	Renamed bool
	By      string
	Note    string
}

// AnswerRename records whether a dropped-and-added column is one column renamed.
//
// Both answers settle the question and only one changes the statements. Saying
// "not a rename" confirms that the drop already in the plan is what was meant,
// which is the more dangerous of the two and so the one most worth having on the
// record with a name against it.
//
// The plan is regenerated either way. An answer of "renamed" has to reach the
// statements, and an answer of "not a rename" must leave a migration whose
// digest reflects a plan decided under it — otherwise an approval could be
// carried over from before the question was settled.
func (s *Scope) AnswerRename(ctx context.Context, actorID, requestID int64, r diff.Rename, renamed bool, note string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	if r.Namespace == "" || r.Table == "" || r.From == "" || r.To == "" {
		return errors.New("which rename question is this answering?")
	}

	// Scoped through the request: an id belonging to another project must be
	// indistinguishable from one that is not there.
	var exists bool
	if err := s.store.pool.QueryRow(ctx, `
		SELECT true FROM schemaver.change_request
		 WHERE id = $1 AND project_id = ANY($2)
		   AND state NOT IN ('CLOSED', 'COMPLETED', 'DONE', 'REVERTED')`,
		requestID, s.projects).Scan(&exists); err != nil {
		return ErrNoSuchRequest
	}

	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO schemaver.rename_decision
		    (change_request_id, namespace, table_name, from_column, to_column,
		     renamed, decided_by, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		ON CONFLICT (change_request_id, namespace, table_name, from_column, to_column)
		DO UPDATE SET renamed = EXCLUDED.renamed, decided_by = EXCLUDED.decided_by,
		              decided_at = now(), note = EXCLUDED.note`,
		requestID, r.Namespace, r.Table, r.From, r.To, renamed, actorID,
		strings.TrimSpace(note)); err != nil {
		return fmt.Errorf("record the answer: %w", err)
	}

	s.record(ctx, Info("request.rename_answered", renameNote(r, renamed)).
		By(actorID).
		OnRequest(requestID))

	return s.regenerate(ctx, actorID, requestID)
}

func renameNote(r diff.Rename, renamed bool) string {
	if renamed {
		return fmt.Sprintf("%s.%s.%s is %s renamed; its data comes with it",
			r.Namespace, r.Table, r.From, r.To)
	}
	return fmt.Sprintf("%s.%s.%s is not %s renamed; its data is discarded",
		r.Namespace, r.Table, r.From, r.To)
}

// ConfirmedRenames reads the answers that say "yes, renamed", which are the
// ones the diff engine acts on.
func (s *Scope) ConfirmedRenames(ctx context.Context, requestID int64) ([]diff.Rename, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT d.namespace, d.table_name, d.from_column, d.to_column
		  FROM schemaver.rename_decision d
		  JOIN schemaver.change_request r ON r.id = d.change_request_id
		 WHERE d.change_request_id = $1 AND d.renamed AND r.project_id = ANY($2)`,
		requestID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read confirmed renames: %w", err)
	}
	defer rows.Close()

	var out []diff.Rename
	for rows.Next() {
		var r diff.Rename
		if err := rows.Scan(&r.Namespace, &r.Table, &r.From, &r.To); err != nil {
			return nil, fmt.Errorf("scan confirmed rename: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RenameAnswers reads every answer on a request, for display.
func (s *Scope) RenameAnswers(ctx context.Context, requestID int64) ([]RenameAnswer, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT d.namespace, d.table_name, d.from_column, d.to_column, d.renamed,
		       COALESCE(u.email, 'removed user'), COALESCE(d.note, '')
		  FROM schemaver.rename_decision d
		  JOIN schemaver.change_request r ON r.id = d.change_request_id
		  LEFT JOIN schemaver.app_user u ON u.id = d.decided_by
		 WHERE d.change_request_id = $1 AND r.project_id = ANY($2)
		 ORDER BY d.namespace, d.table_name, d.from_column`, requestID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read rename answers: %w", err)
	}
	defer rows.Close()

	var out []RenameAnswer
	for rows.Next() {
		var a RenameAnswer
		if err := rows.Scan(&a.Namespace, &a.Table, &a.From, &a.To, &a.Renamed,
			&a.By, &a.Note); err != nil {
			return nil, fmt.Errorf("scan rename answer: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// answeredCount is how many of a migration's rename candidates have been
// settled, so the gate can stop counting unanswered ones by their absence.
func (s *Store) answeredRenames(ctx context.Context, requestID int64) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT namespace || '.' || table_name || '.' || from_column || '>' || to_column
		  FROM schemaver.rename_decision WHERE change_request_id = $1`, requestID)
	if err != nil {
		return nil, fmt.Errorf("read answered renames: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan answered rename: %w", err)
		}
		out[key] = true
	}
	return out, rows.Err()
}

// renameKey identifies a candidate and its answer by the same string.
func renameKey(ns, table, from, to string) string {
	return ns + "." + table + "." + from + ">" + to
}

// regenerate rebuilds a request's migration from wherever it came from.
//
// A change request has three origins and each rebuilds differently: a
// comparison against another database re-diffs the two, a branch merge re-plans
// the merge, and a written change re-runs its statements against a throwaway
// copy. What they share is that the plan is derived rather than stored, so
// anything that changes how it is derived — a rename question being answered,
// above all — has to go back to the source rather than edit the statements.
//
// The request is kept. Opening a second one would leave the discussion, the
// questions and the history on the first.
func (s *Scope) regenerate(ctx context.Context, actorID, requestID int64) error {
	var sourceID, branchID, databaseID *int64
	var authored *string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT r.source_database_id, r.branch_id, r.database_id, r.authored_sql
		  FROM schemaver.change_request r
		 WHERE r.id = $1 AND r.project_id = ANY($2)`, requestID, s.projects).
		Scan(&sourceID, &branchID, &databaseID, &authored); err != nil {
		return ErrNoSuchRequest
	}

	switch {
	case sourceID != nil:
		_, err := s.GenerateMigration(ctx, actorID, requestID)
		return err

	case branchID != nil && databaseID != nil:
		m, err := s.PlanBranchMerge(ctx, *branchID, *databaseID)
		if err != nil {
			return err
		}
		if !m.Clean() {
			return fmt.Errorf("this merge now conflicts and cannot be rebuilt: %s",
				m.Conflicts[0].Describe())
		}
		return s.recordBranchMerge(ctx, requestID, m)

	case authored != nil:
		// The statements are the author's and do not change; what they produce
		// is worked out again by running them, which is the worker's job.
		return s.store.EnqueueDerivation(ctx, requestID)
	}
	return errors.New("this request has nothing to rebuild from")
}
