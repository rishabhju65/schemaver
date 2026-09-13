package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/render"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ErrNoSuchBranch is returned for a branch that is not there, or belongs to
// another project. The two are deliberately indistinguishable.
var ErrNoSuchBranch = errors.New("no such branch")

// Branch is a line of schema development cut from a database.
type Branch struct {
	ID          int64
	Name        string
	Description string

	// Origin names the database it was cut from, empty if that database has
	// since been removed. Base is the schema at the cut and never moves; Head
	// is where the branch is now.
	Origin     string
	OriginID   int64
	Base, Head schema.Version

	Commits   int
	CreatedBy string
	CreatedAt time.Time

	// WriteError is why the last write did not apply, cleared by the next one
	// that does.
	WriteError string

	ClosedAt     *time.Time
	ClosedBy     string
	ClosedReason string
}

// Open reports a branch that can still be written to.
func (b *Branch) Open() bool { return b.ClosedAt == nil }

// Diverged reports that the branch has moved since it was cut.
func (b *Branch) Diverged() bool { return b.Head != b.Base }

// BranchCommit is one write that landed on a branch.
type BranchCommit struct {
	Ordinal  int
	From, To schema.Version
	SQL      string
	Changes  []diff.Change
	Message  string
	Author   string
	At       time.Time
}

// CutBranch starts a line of development from a database's current schema.
//
// The schema is taken from where the database is *now* and recorded as the
// branch's base, which is what makes merging back exact rather than
// reconstructed: D-027 has to recover a common ancestor from two databases'
// snapshot histories, and a branch simply knows.
//
// The database itself is not touched, held, or locked. It carries on being read
// and changed while the branch exists, and the branch does not notice — that is
// what independent means here.
func (s *Scope) CutBranch(ctx context.Context, actorID, databaseID int64, name, description string) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("a branch needs a name")
	}

	var fingerprint *string
	var origin string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT d.current_fingerprint, d.name
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1 AND i.project_id = ANY($2)`, databaseID, s.projects).
		Scan(&fingerprint, &origin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("no such database")
		}
		return 0, fmt.Errorf("read the database to branch from: %w", err)
	}
	if fingerprint == nil {
		// Nothing to cut from. Branching at "unknown" would produce a base that
		// is not a schema, and every later merge would have no ancestor.
		return 0, fmt.Errorf(
			"%s has not been read yet, so there is no schema to branch from", origin)
	}

	var id int64
	err := s.store.pool.QueryRow(ctx, `
		INSERT INTO schemaver.branch
		    (project_id, name, description, origin_database_id,
		     base_fingerprint, head_fingerprint, created_by)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $5, $6)
		RETURNING id`,
		s.writable, name, strings.TrimSpace(description), databaseID,
		*fingerprint, actorID).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "branch_name_is_unique_while_open") {
			return 0, fmt.Errorf("a branch called %q is already open", name)
		}
		return 0, fmt.Errorf("create the branch: %w", err)
	}
	return id, nil
}

const branchColumns = `
	    b.id, b.name, COALESCE(b.description, ''),
	    COALESCE(d.name, ''), COALESCE(b.origin_database_id, 0),
	    b.base_fingerprint, b.head_fingerprint,
	    (SELECT count(*) FROM schemaver.branch_commit c WHERE c.branch_id = b.id),
	    COALESCE(u.email, 'removed user'), b.created_at,
	    COALESCE(b.write_error, ''),
	    b.closed_at, COALESCE(cb.email, ''), COALESCE(b.closed_reason, '')`

func scanBranch(row pgx.Row) (*Branch, error) {
	var b Branch
	var base, head string
	if err := row.Scan(&b.ID, &b.Name, &b.Description, &b.Origin, &b.OriginID,
		&base, &head, &b.Commits, &b.CreatedBy, &b.CreatedAt, &b.WriteError,
		&b.ClosedAt, &b.ClosedBy, &b.ClosedReason); err != nil {
		return nil, err
	}
	b.Base, b.Head = schema.Version(base), schema.Version(head)
	return &b, nil
}

// Branches lists this project's branches, open ones first.
func (s *Scope) Branches(ctx context.Context) ([]*Branch, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT`+branchColumns+`
		  FROM schemaver.branch b
		  LEFT JOIN schemaver.database d ON d.id = b.origin_database_id
		  LEFT JOIN schemaver.app_user u ON u.id = b.created_by
		  LEFT JOIN schemaver.app_user cb ON cb.id = b.closed_by
		 WHERE b.project_id = ANY($1)
		 ORDER BY b.closed_at IS NOT NULL, b.created_at DESC`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	var out []*Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Branch loads one branch.
func (s *Scope) Branch(ctx context.Context, id int64) (*Branch, error) {
	b, err := scanBranch(s.store.pool.QueryRow(ctx, `
		SELECT`+branchColumns+`
		  FROM schemaver.branch b
		  LEFT JOIN schemaver.database d ON d.id = b.origin_database_id
		  LEFT JOIN schemaver.app_user u ON u.id = b.created_by
		  LEFT JOIN schemaver.app_user cb ON cb.id = b.closed_by
		 WHERE b.id = $1 AND b.project_id = ANY($2)`, id, s.projects))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSuchBranch
	}
	if err != nil {
		return nil, fmt.Errorf("load branch %d: %w", id, err)
	}
	return b, nil
}

// BranchCommits reads what has happened on a branch, newest first.
func (s *Scope) BranchCommits(ctx context.Context, branchID int64) ([]BranchCommit, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT c.ordinal, c.from_fingerprint, c.to_fingerprint, c.sql,
		       c.changes, COALESCE(c.message, ''), COALESCE(u.email, 'removed user'),
		       c.created_at
		  FROM schemaver.branch_commit c
		  JOIN schemaver.branch b ON b.id = c.branch_id
		  LEFT JOIN schemaver.app_user u ON u.id = c.author_id
		 WHERE c.branch_id = $1 AND b.project_id = ANY($2)
		 ORDER BY c.ordinal DESC`, branchID, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read the branch history: %w", err)
	}
	defer rows.Close()

	var out []BranchCommit
	for rows.Next() {
		var c BranchCommit
		var from, to string
		var changesJSON []byte
		if err := rows.Scan(&c.Ordinal, &from, &to, &c.SQL, &changesJSON,
			&c.Message, &c.Author, &c.At); err != nil {
			return nil, fmt.Errorf("scan branch commit: %w", err)
		}
		c.From, c.To = schema.Version(from), schema.Version(to)
		if err := json.Unmarshal(changesJSON, &c.Changes); err != nil {
			return nil, fmt.Errorf("decode branch commit changes: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BranchDiff is what the branch has done since it was cut.
//
// The question the whole feature exists to answer: evolve a schema
// independently, then see exactly what diverged. Computed from the two stored
// schemas rather than accumulated across commits, so a change made and then
// undone does not appear — what is wanted is the difference, not the journey.
func (s *Scope) BranchDiff(ctx context.Context, branchID int64) (diff.Result, error) {
	b, err := s.Branch(ctx, branchID)
	if err != nil {
		return diff.Result{}, err
	}
	base, err := s.Blob(ctx, b.Base)
	if err != nil {
		return diff.Result{}, fmt.Errorf("read the schema this branch was cut from: %w", err)
	}
	head, err := s.Blob(ctx, b.Head)
	if err != nil {
		return diff.Result{}, fmt.Errorf("read the branch's schema: %w", err)
	}
	return diff.Compute(base, head), nil
}

// CloseBranch ends a branch. Everything it recorded stays readable.
func (s *Scope) CloseBranch(ctx context.Context, actorID, branchID int64, reason string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	tag, err := s.store.pool.Exec(ctx, `
		UPDATE schemaver.branch
		   SET closed_at = now(), closed_by = $2, closed_reason = NULLIF($3, '')
		 WHERE id = $1 AND project_id = ANY($4) AND closed_at IS NULL`,
		branchID, actorID, strings.TrimSpace(reason), s.projects)
	if err != nil {
		return fmt.Errorf("close branch %d: %w", branchID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchBranch
	}
	return nil
}

// WriteToBranch queues somebody's DDL against a branch.
//
// Queued rather than applied here because applying it means standing up a
// throwaway database, running the statements and reading the result back, which
// is the worker's job and takes seconds rather than milliseconds. The write is
// recorded as pending by clearing any previous error; what it did is recorded
// when it lands.
//
// Only one write is in flight per branch at a time. The statements are composed
// against the head, so two overlapping writes would both build on the schema
// before either landed, and the second would either fail confusingly or quietly
// undo the first.
func (s *Scope) WriteToBranch(ctx context.Context, actorID, branchID int64, sql, message string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(sql) == "" {
		return errors.New("write the statements this branch should take on")
	}

	b, err := s.Branch(ctx, branchID)
	if err != nil {
		return err
	}
	if !b.Open() {
		return errors.New("this branch is closed; nothing further can be written to it")
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.branch
		   SET pending_sql = $3, pending_message = NULLIF($4, ''),
		       pending_author = $5, write_error = NULL
		 WHERE id = $1 AND project_id = ANY($2)`,
		branchID, s.projects, sql, strings.TrimSpace(message), actorID); err != nil {
		return fmt.Errorf("record the write: %w", err)
	}

	// No instance: this touches the shadow server only, so charging it against
	// a real instance's connection budget would hold up work on that instance
	// to pay for connections it never opens.
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.job
		    (kind, target_kind, target_id, weight, idempotency_key)
		VALUES ($1, 'branch', $2, 1, $3)
		ON CONFLICT (idempotency_key) DO UPDATE
		   SET state = 'pending', run_after = now(), error = NULL,
		       finished_at = NULL`,
		KindBranchWrite, branchID, fmt.Sprintf("branch_write:%d", branchID)); err != nil {
		return fmt.Errorf("queue the write: %w", err)
	}
	return tx.Commit(ctx)
}

// BranchWriteTask is everything the worker needs to apply a pending write.
type BranchWriteTask struct {
	BranchID   int64
	Name       string
	Head       schema.Version
	BaseDDL    string
	Statements []string
	SQL        string
	Message    string
	AuthorID   int64

	// BaseSchema is the branch's current schema, decoded on the way to
	// rendering the DDL. Kept so the worker can classify what the write did
	// without reading the same blob back a second time.
	BaseSchema *schema.Schema
}

// LoadBranchWrite assembles a pending write.
//
// Unscoped, like the other worker loaders: a worker serves every project at
// once and holds no scope. Resolving the branch id is what establishes which
// project this belongs to, and nothing here is returned to a caller who could
// have asked for a different one.
func (s *Store) LoadBranchWrite(ctx context.Context, branchID int64) (*BranchWriteTask, error) {
	t := &BranchWriteTask{BranchID: branchID}
	var head string
	var canonical []byte
	var author *int64
	var message *string
	if err := s.pool.QueryRow(ctx, `
		SELECT b.name, b.head_fingerprint, b.pending_sql, b.pending_message,
		       b.pending_author, blob.canonical
		  FROM schemaver.branch b
		  JOIN schemaver.schema_blob blob ON blob.fingerprint = b.head_fingerprint
		 WHERE b.id = $1 AND b.pending_sql IS NOT NULL AND b.closed_at IS NULL`,
		branchID).Scan(&t.Name, &head, &t.SQL, &message, &author, &canonical); err != nil {
		return nil, fmt.Errorf("load the pending write: %w", err)
	}
	t.Head = schema.Version(head)
	t.Statements = SplitStatements(t.SQL)
	if message != nil {
		t.Message = *message
	}
	if author != nil {
		t.AuthorID = *author
	}

	// Rendered to DDL rather than handed over as the canonical JSON it is
	// stored as: the shadow rebuilds a schema by executing statements.
	var base schema.Schema
	if err := json.Unmarshal(canonical, &base); err != nil {
		return nil, fmt.Errorf("decode the branch's schema: %w", err)
	}
	t.BaseDDL = render.Schema(&base)
	t.BaseSchema = &base
	return t, nil
}

// FailBranchWrite records that a write did not apply, and clears it.
//
// The statements are dropped rather than left pending, because a pending write
// is one that is going to be tried: leaving a failed one there would have the
// branch page showing work about to happen that never will, and the next write
// would silently replace it anyway.
func (s *Store) FailBranchWrite(ctx context.Context, branchID int64, reason string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE schemaver.branch
		   SET write_error = $2, pending_sql = NULL, pending_message = NULL,
		       pending_author = NULL
		 WHERE id = $1`, branchID, reason); err != nil {
		return fmt.Errorf("record the failed write: %w", err)
	}
	return nil
}

// RecordBranchWrite stores what a write turned out to do and advances the head.
//
// The schema is stored first and the head moved in the same transaction as the
// commit, so a branch never points at a schema nobody kept, and never gains a
// commit whose endpoint it is not at.
func (s *Store) RecordBranchWrite(ctx context.Context, t *BranchWriteTask, to schema.Version, sch *schema.Schema, changes []diff.Change) error {
	if err := s.StoreSchema(ctx, sch, to); err != nil {
		return err
	}
	changesJSON, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("serialize changes: %w", err)
	}
	if len(changes) == 0 {
		changesJSON = []byte("[]")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Conditional on the head not having moved. Nothing else should be writing
	// to this branch, and if something was, the statements were composed
	// against a schema the branch is no longer at.
	tag, err := tx.Exec(ctx, `
		UPDATE schemaver.branch
		   SET head_fingerprint = $2, pending_sql = NULL, pending_message = NULL,
		       pending_author = NULL, write_error = NULL
		 WHERE id = $1 AND head_fingerprint = $3`,
		t.BranchID, string(to), string(t.Head))
	if err != nil {
		return fmt.Errorf("advance the branch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("branch %d moved while this write was being applied", t.BranchID)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.branch_commit
		    (branch_id, ordinal, from_fingerprint, to_fingerprint, sql, changes,
		     message, author_id)
		VALUES ($1,
		        (SELECT COALESCE(max(ordinal), 0) + 1 FROM schemaver.branch_commit
		          WHERE branch_id = $1),
		        $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, 0))`,
		t.BranchID, string(t.Head), string(to), t.SQL, changesJSON,
		t.Message, t.AuthorID); err != nil {
		return fmt.Errorf("record the branch commit: %w", err)
	}
	return tx.Commit(ctx)
}
