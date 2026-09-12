package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Propose creates a change request to bring one database in line with another.
//
// Per D-018 the target is an existing database's schema rather than authored
// intent, which is why no repository is involved: the desired state already
// exists, has already been introspected, and is already content-addressed.
func (s *Scope) Propose(ctx context.Context, authorID, databaseID, sourceID int64, title, description string) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	if databaseID == sourceID {
		return 0, errors.New("a database cannot be brought in line with itself")
	}
	if title == "" {
		return 0, errors.New("a change request needs a title")
	}

	// Both databases must be reachable from this scope, so ids cannot be probed
	// for existence across projects.
	var reachable int
	if err := s.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = ANY($1) AND i.project_id = ANY($2)`,
		[]int64{databaseID, sourceID}, s.projects).Scan(&reachable); err != nil {
		return 0, fmt.Errorf("check databases: %w", err)
	}
	if reachable != 2 {
		return 0, errors.New("no such database")
	}
	// Reachable is not the same as writable. Both ends must still be databases
	// schemaver is watching: the target because the change is aimed at it, and
	// the source because its schema is what the target will be made to match,
	// and matching a schema nobody has read since it was stood down is a
	// promise about a state we cannot vouch for.
	if err := s.requireWritable(ctx, databaseID, sourceID); err != nil {
		return 0, err
	}

	var id int64
	if err := s.store.pool.QueryRow(ctx, `
		INSERT INTO schemaver.change_request
		    (project_id, title, description, author_id, database_id, source_database_id)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6)
		RETURNING id`,
		s.writable, title, description, authorID, databaseID, sourceID).Scan(&id); err != nil {
		return 0, fmt.Errorf("create change request: %w", err)
	}
	s.record(ctx, Info("request.opened", title).
		By(authorID).
		OnRequest(id).
		OnDatabase(databaseID))
	return id, nil
}

// ErrNothingToDo is returned when the two schemas already agree.
var ErrNothingToDo = errors.New("the target database already matches the source schema")

// ErrNotObserved is returned when a schema has never been read.
var ErrNotObserved = errors.New("one of these databases has not been read yet")

// GenerateMigration diffs the request's target against its source and stores the
// result.
//
// Regenerating supersedes the previous migration rather than replacing it, so
// what a reviewer approved stays inspectable — and because approvals record the
// fingerprint pair they concerned, superseding one silently withdraws its
// approvals without anything having to invalidate them.
func (s *Scope) GenerateMigration(ctx context.Context, actorID, requestID int64) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}

	var fromFP, toFP *string
	err := s.store.pool.QueryRow(ctx, `
		SELECT target.current_fingerprint, source.current_fingerprint
		  FROM schemaver.change_request r
		  JOIN schemaver.database target ON target.id = r.database_id
		  JOIN schemaver.database source ON source.id = r.source_database_id
		 WHERE r.id = $1 AND r.project_id = ANY($2)`, requestID, s.projects).
		Scan(&fromFP, &toFP)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("no such change request, or it has no source database")
	}
	if err != nil {
		return 0, fmt.Errorf("load request: %w", err)
	}
	if fromFP == nil || toFP == nil {
		return 0, ErrNotObserved
	}
	if *fromFP == *toFP {
		return 0, ErrNothingToDo
	}

	from, err := s.Blob(ctx, schema.Version(*fromFP))
	if err != nil {
		return 0, err
	}
	to, err := s.Blob(ctx, schema.Version(*toFP))
	if err != nil {
		return 0, err
	}

	result := diff.Compute(from, to)
	if result.Empty() {
		// Different fingerprints with no changes would mean the diff engine and
		// the canonical model disagree, which is a bug rather than a no-op.
		return 0, fmt.Errorf(
			"schemas fingerprint differently (%s vs %s) but no changes were found; "+
				"this is a defect in the diff engine",
			schema.Version(*fromFP).Short(), schema.Version(*toFP).Short())
	}
	statements := render.Statements(result.Changes, from, to)

	changesJSON, err := json.Marshal(result.Changes)
	if err != nil {
		return 0, fmt.Errorf("serialize changes: %w", err)
	}
	// An empty slice rather than nil: json.Marshal(nil) writes the JSON scalar
	// `null`, which is not an array, and every reader that counts the
	// candidates would then fail rather than see zero of them.
	renames := result.Renames
	if renames == nil {
		renames = []diff.RenameCandidate{}
	}
	renamesJSON, err := json.Marshal(renames)
	if err != nil {
		return 0, fmt.Errorf("serialize rename candidates: %w", err)
	}

	// No revert is generated. Whoever writes the change writes the way back
	// (D-022), so the digest covers the forward statements and whatever revert
	// has been authored so far — which at generation time is none.
	forwardSteps := make([]Step, 0, len(statements))
	for i, st := range statements {
		forwardSteps = append(forwardSteps, Step{
			Ordinal: i + 1, SQL: st.SQL, ChangeID: st.ChangeID,
			Transactional: st.Transactional, Note: st.Note,
		})
	}
	digest := planDigest(forwardSteps, nil)

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.migration SET superseded_at = now()
		 WHERE change_request_id = $1 AND superseded_at IS NULL`, requestID); err != nil {
		return 0, fmt.Errorf("supersede previous migration: %w", err)
	}

	var migrationID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.migration
		    (change_request_id, from_fingerprint, to_fingerprint, changes,
		     rename_candidates, irreversible_reason, weight, plan_digest)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)
		RETURNING id`,
		requestID, *fromFP, *toFP, changesJSON, renamesJSON,
		irreversibleReason(result), diff.Weight(result.Changes),
		digest).Scan(&migrationID); err != nil {
		return 0, fmt.Errorf("store migration: %w", err)
	}

	for i, st := range statements {
		// expected_after is left null: the fingerprint chain needs each step
		// simulated in a shadow database, which the executor will do when it
		// needs the precision. Recovery falls back to the endpoints until then,
		// which is safe but less informative (D-013).
		if _, err := tx.Exec(ctx, `
			INSERT INTO schemaver.migration_step
			    (migration_id, ordinal, sql, change_id, transactional, note)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
			migrationID, i+1, st.SQL, st.ChangeID, st.Transactional, st.Note); err != nil {
			return 0, fmt.Errorf("store step %d: %w", i+1, err)
		}
	}

	// Straight to review. The shadow runs after approval rather than before it
	// (D-022): with a revert somebody has to write, review is about judgement,
	// and the staging run belongs where it is the last gate before production
	// rather than a precondition of reading.
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'IN_REVIEW', state_reason = NULL, updated_at = now()
		 WHERE id = $1`, requestID); err != nil {
		return 0, fmt.Errorf("advance request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	// Recorded with the risk breakdown, because "generated 14 statements" and
	// "generated 14 statements, 2 of them destructive" are different news.
	generated := Info("migration.generated", fmt.Sprintf(
		"%d change(s) in %d statement(s), %s → %s",
		len(result.Changes), len(statements),
		schema.Version(*fromFP).Short(), schema.Version(*toFP).Short()))
	if result.Summary.Destructive > 0 {
		generated = Warn("migration.generated", fmt.Sprintf(
			"%d change(s) in %d statement(s), %d of them destructive, %s → %s",
			len(result.Changes), len(statements), result.Summary.Destructive,
			schema.Version(*fromFP).Short(), schema.Version(*toFP).Short()))
	}
	s.record(ctx, generated.
		By(actorID).
		OnRequest(requestID).
		OnMigration(migrationID).
		With(map[string]any{
			"statements": len(statements), "summary": result.Summary,
			"from": *fromFP, "to": *toFP,
		}))
	return migrationID, nil
}

// irreversibleReason names why a migration cannot be undone, so D-012's promise
// that irreversibility is surfaced at approval time has something to surface.
func irreversibleReason(r diff.Result) string {
	var destructive []string
	for _, c := range r.Changes {
		if c.Class == diff.Destructive {
			destructive = append(destructive, c.Qualified())
		}
	}
	if len(destructive) == 0 {
		return ""
	}
	if len(destructive) == 1 {
		return fmt.Sprintf("discards %s; no generated DDL restores its contents", destructive[0])
	}
	return fmt.Sprintf("discards %d objects including %s; no generated DDL restores their contents",
		len(destructive), destructive[0])
}
