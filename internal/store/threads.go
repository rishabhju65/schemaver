package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Thread is a discussion about one change, or about a request as a whole.
type Thread struct {
	ID         int64
	Anchor     string // a semantic change identity; empty means the request itself
	Status     string
	Resolution string
	CreatedBy  string
	CreatedAt  time.Time
	Comments   []Comment

	// Orphaned reports that the change this thread is about is no longer in the
	// current migration. That is usually the objection having been acted on, and
	// it is why the anchor is not a foreign key.
	Orphaned bool
}

// Comment is one message in a thread.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	Edited    bool
}

// StartThread opens a discussion, optionally anchored to a change.
//
// The anchor is not validated against the current migration. A reviewer may
// comment on a change that is about to disappear, and refusing that would be
// both surprising and unnecessary — the thread simply becomes orphaned, which is
// meaningful.
func (s *Scope) StartThread(ctx context.Context, requestID, userID int64, anchor, body string) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	if body == "" {
		return 0, errors.New("a thread needs something in it")
	}
	if err := s.ownsRequest(ctx, requestID); err != nil {
		return 0, err
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.review_thread (change_request_id, anchor, created_by)
		VALUES ($1, NULLIF($2, ''), $3) RETURNING id`,
		requestID, anchor, userID).Scan(&id); err != nil {
		return 0, fmt.Errorf("open thread: %w", err)
	}
	if err := addComment(ctx, tx, id, userID, body); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// Reply adds a comment to an existing thread.
func (s *Scope) Reply(ctx context.Context, threadID, userID int64, body string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	if body == "" {
		return errors.New("a reply needs something in it")
	}
	// Reached through the thread's request, so a thread in another project is
	// not repliable by id.
	var requestID int64
	if err := s.store.pool.QueryRow(ctx, `
		SELECT t.change_request_id FROM schemaver.review_thread t
		  JOIN schemaver.change_request r ON r.id = t.change_request_id
		 WHERE t.id = $1 AND r.project_id = ANY($2)`, threadID, s.projects).Scan(&requestID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no such thread")
		}
		return fmt.Errorf("load thread: %w", err)
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := addComment(ctx, tx, threadID, userID, body); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// addComment writes a comment with its author's identity denormalised, so it
// still reads sensibly once that account is gone.
func addComment(ctx context.Context, tx pgx.Tx, threadID, userID int64, body string) error {
	var label string
	if err := tx.QueryRow(ctx,
		`SELECT email FROM schemaver.app_user WHERE id = $1`, userID).Scan(&label); err != nil {
		return fmt.Errorf("load author: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.review_comment (thread_id, author_id, author_label, body)
		VALUES ($1, $2, $3, $4)`, threadID, userID, label, body); err != nil {
		return fmt.Errorf("add comment: %w", err)
	}
	return nil
}

// ResolveThread closes a thread with a stated reason.
func (s *Scope) ResolveThread(ctx context.Context, threadID, userID int64, resolution string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	switch resolution {
	case "addressed", "change_removed", "declined":
	default:
		return fmt.Errorf("unknown resolution %q", resolution)
	}
	tag, err := s.store.pool.Exec(ctx, `
		UPDATE schemaver.review_thread t
		   SET status = 'resolved', resolution = $3, resolved_by = $2, resolved_at = now()
		 WHERE t.id = $1 AND t.status = 'open'
		   AND EXISTS (SELECT 1 FROM schemaver.change_request r
		                WHERE r.id = t.change_request_id AND r.project_id = ANY($4))`,
		threadID, userID, resolution, s.projects)
	if err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("no such open thread")
	}
	return nil
}

// Threads returns every thread on a request, with its comments.
//
// A thread whose anchor is absent from the current migration is marked orphaned
// rather than hidden. Hiding it would lose the record of an objection, and the
// most likely reason it is orphaned is that the objection was acted on — which
// is precisely what a reader wants to see.
func (s *Scope) Threads(ctx context.Context, requestID int64) ([]Thread, error) {
	if err := s.ownsRequest(ctx, requestID); err != nil {
		return nil, err
	}

	rows, err := s.store.pool.Query(ctx, `
		SELECT t.id, COALESCE(t.anchor, ''), t.status, COALESCE(t.resolution, ''),
		       COALESCE(u.email, 'removed user'), t.created_at,
		       CASE
		         WHEN t.anchor IS NULL THEN false
		         ELSE NOT EXISTS (
		           SELECT 1 FROM schemaver.migration m,
		                        jsonb_array_elements(m.changes) c
		            WHERE m.change_request_id = t.change_request_id
		              AND m.superseded_at IS NULL
		              AND c->>'id' = t.anchor)
		       END AS orphaned
		  FROM schemaver.review_thread t
		  LEFT JOIN schemaver.app_user u ON u.id = t.created_by
		 WHERE t.change_request_id = $1
		 ORDER BY (t.status = 'open') DESC, t.created_at`, requestID)
	if err != nil {
		return nil, fmt.Errorf("load threads: %w", err)
	}
	defer rows.Close()

	var out []Thread
	byID := map[int64]int{}
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.Anchor, &t.Status, &t.Resolution,
			&t.CreatedBy, &t.CreatedAt, &t.Orphaned); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		byID[t.ID] = len(out)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	comments, err := s.store.pool.Query(ctx, `
		SELECT c.thread_id, c.id, c.author_label, c.body, c.created_at,
		       c.edited_at IS NOT NULL
		  FROM schemaver.review_comment c
		  JOIN schemaver.review_thread t ON t.id = c.thread_id
		 WHERE t.change_request_id = $1
		 ORDER BY c.created_at`, requestID)
	if err != nil {
		return nil, fmt.Errorf("load comments: %w", err)
	}
	defer comments.Close()

	for comments.Next() {
		var threadID int64
		var c Comment
		if err := comments.Scan(&threadID, &c.ID, &c.Author, &c.Body,
			&c.CreatedAt, &c.Edited); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		if i, ok := byID[threadID]; ok {
			out[i].Comments = append(out[i].Comments, c)
		}
	}
	return out, comments.Err()
}

// ownsRequest confirms a request belongs to a project this scope can reach, so
// a request id in a URL is never trusted on its own.
func (s *Scope) ownsRequest(ctx context.Context, requestID int64) error {
	var exists bool
	if err := s.store.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM schemaver.change_request
		                WHERE id = $1 AND project_id = ANY($2))`,
		requestID, s.projects).Scan(&exists); err != nil {
		return fmt.Errorf("check request ownership: %w", err)
	}
	if !exists {
		return errors.New("no such change request")
	}
	return nil
}
