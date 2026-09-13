package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ErrNothingWritten is returned when an authored change contains no statements.
var ErrNothingWritten = errors.New(
	"this change has no statements; write the SQL that makes the change you want")

// ProposeAuthored opens a change request for SQL somebody wrote, rather than for
// a difference between two databases.
//
// The target fingerprint is not taken on trust and is not asked for. It is
// derived later by applying these statements to a throwaway database built at
// the target's current schema and reading what results — so a written migration
// is held to exactly the standard a generated one is, and arrives in review
// with its changes classified the same way.
//
// Deriving it is the worker's job rather than this one's. Building a shadow
// means creating a database, loading a whole schema into it and introspecting
// the result, which takes seconds at best and much longer on a large schema;
// holding an HTTP request open for that would make proposing feel broken and
// would give the work no retry.
func (s *Scope) ProposeAuthored(ctx context.Context, authorID, databaseID int64, title, description, sql string) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(title) == "" {
		return 0, errors.New("a change request needs a title")
	}
	if len(SplitStatements(sql)) == 0 {
		return 0, ErrNothingWritten
	}
	if err := s.requireWritable(ctx, databaseID); err != nil {
		return 0, err
	}

	// Derivation starts from the schema last observed on the target, so there
	// has to be one. Without it there is nothing to apply the statements to.
	var observed bool
	if err := s.store.pool.QueryRow(ctx, `
		SELECT d.current_fingerprint IS NOT NULL
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1 AND i.project_id = ANY($2)`,
		databaseID, s.projects).Scan(&observed); err != nil {
		return 0, fmt.Errorf("check the database has been read: %w", err)
	}
	if !observed {
		return 0, errors.New(
			"this database has not been read yet, so there is nothing to apply " +
				"the change to; it is read on the next observation cycle")
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.change_request
		    (project_id, title, description, author_id, database_id, authored_sql,
		     state, state_reason)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, 'INITIATED',
		        'working out what these statements do')
		RETURNING id`,
		s.writable, title, description, authorID, databaseID, sql).Scan(&id); err != nil {
		return 0, fmt.Errorf("create change request: %w", err)
	}

	// No instance: deriving touches the shadow server and never the database
	// being changed, so charging it against that instance's connection budget
	// would hold up real work to pay for connections it does not open.
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, weight, idempotency_key)
		VALUES ('derive', 'request', $1, 1, $2)
		ON CONFLICT (idempotency_key) DO UPDATE
		   SET state = 'pending', run_after = now(), error = NULL,
		       finished_at = NULL`,
		id, fmt.Sprintf("derive:%d", id)); err != nil {
		return 0, fmt.Errorf("queue the derivation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	s.record(ctx, Info("request.written", title).
		By(authorID).
		OnRequest(id).
		OnDatabase(databaseID).
		With(map[string]any{"statements": len(SplitStatements(sql))}))
	return id, nil
}

// AuthoredTask is what the worker needs to work out what written SQL does.
type AuthoredTask struct {
	RequestID  int64
	DatabaseID int64
	ProjectID  int64
	// BaseDDL rebuilds the target's current schema in a throwaway database, and
	// From is the fingerprint it should come out as. BaseSchema is the same
	// schema already decoded, so the caller diffing against it does not read and
	// parse the blob a second time.
	BaseDDL    string
	BaseSchema *schema.Schema
	From       string
	Statements []string
}

// LoadAuthoredTask assembles a derivation.
func (s *Store) LoadAuthoredTask(ctx context.Context, requestID int64) (*AuthoredTask, error) {
	t := &AuthoredTask{RequestID: requestID}
	var sql string
	var canonical []byte
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, i.project_id, d.current_fingerprint, r.authored_sql, b.canonical
		  FROM schemaver.change_request r
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  JOIN schemaver.schema_blob b ON b.fingerprint = d.current_fingerprint
		 WHERE r.id = $1 AND r.authored_sql IS NOT NULL
		   AND r.state NOT IN ('CLOSED', 'COMPLETED')`, requestID).
		Scan(&t.DatabaseID, &t.ProjectID, &t.From, &sql, &canonical)
	if err != nil {
		return nil, fmt.Errorf("load authored change: %w", err)
	}
	t.Statements = SplitStatements(sql)

	// Rendered to DDL, not handed over as the canonical JSON it is stored as.
	// The shadow rebuilds a schema by executing statements, so what it needs is
	// the schema written out as SQL.
	var base schema.Schema
	if err := json.Unmarshal(canonical, &base); err != nil {
		return nil, fmt.Errorf("decode the starting schema: %w", err)
	}
	t.BaseDDL = render.Schema(&base)
	t.BaseSchema = &base
	return t, nil
}

// StoreSchema records a canonical schema against its fingerprint.
//
// Separate from observation, which stores a blob as part of recording that a
// real database was seen in that state. A derived schema was never on any
// database — it exists because statements were applied to a throwaway copy —
// but the migration that targets it needs it stored, because the fingerprint
// columns reference the blob table and because review reads both ends back.
func (s *Store) StoreSchema(ctx context.Context, sch *schema.Schema, fingerprint schema.Version) error {
	canonical, err := schema.Canonical(sch)
	if err != nil {
		return fmt.Errorf("serialize schema: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.schema_blob (fingerprint, canonical, table_count, bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fingerprint) DO NOTHING`,
		string(fingerprint), canonical, tableCount(sch), len(canonical)); err != nil {
		return fmt.Errorf("store schema blob: %w", err)
	}
	return nil
}

// RecordDerivation stores what written statements turn out to do.
//
// Creates the migration the same shape a generated one has — a fingerprint
// pair, a classified change list, and the statements in order — so everything
// downstream cannot tell the difference and does not need to. Review, the
// approval gate, the rehearsal, execution and rollback all work on that shape
// alone.
func (s *Store) RecordDerivation(ctx context.Context, requestID int64, from, to schema.Version, result diff.Result, statements []string, weight int) (int64, error) {
	changesJSON, err := json.Marshal(result.Changes)
	if err != nil {
		return 0, fmt.Errorf("serialize changes: %w", err)
	}
	if len(result.Changes) == 0 {
		changesJSON = []byte("[]")
	}
	renamesJSON, err := json.Marshal([]diff.RenameCandidate{})
	if err != nil {
		return 0, fmt.Errorf("serialize rename candidates: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
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
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, '')
		RETURNING id`,
		requestID, string(from), string(to), changesJSON, renamesJSON,
		irreversibleReason(result), weight).Scan(&migrationID); err != nil {
		return 0, fmt.Errorf("store migration: %w", err)
	}

	forward := make([]Step, 0, len(statements))
	for i, sql := range statements {
		st := Step{Ordinal: i + 1, SQL: sql, ChangeID: "authored",
			Transactional: !concurrent(sql)}
		forward = append(forward, st)
		if _, err := tx.Exec(ctx, `
			INSERT INTO schemaver.migration_step
			    (migration_id, ordinal, sql, change_id, transactional)
			VALUES ($1, $2, $3, $4, $5)`,
			migrationID, st.Ordinal, st.SQL, st.ChangeID, st.Transactional); err != nil {
			return 0, fmt.Errorf("store statement %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE schemaver.migration SET plan_digest = $2 WHERE id = $1`,
		migrationID, planDigest(forward, nil)); err != nil {
		return 0, fmt.Errorf("record the plan digest: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'IN_REVIEW', state_reason = NULL, updated_at = now()
		 WHERE id = $1`, requestID); err != nil {
		return 0, fmt.Errorf("advance request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return migrationID, nil
}

// FailDerivation records that written statements could not be made sense of,
// and says so on the request rather than leaving it silent.
func (s *Store) FailDerivation(ctx context.Context, requestID int64, reason string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = 'CHANGES_REQUESTED', state_reason = $2, updated_at = now()
		 WHERE id = $1`, requestID, reason); err != nil {
		return fmt.Errorf("record the failed derivation: %w", err)
	}
	return nil
}
