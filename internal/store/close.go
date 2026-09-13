package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRunning is returned when a request cannot be settled because something is
// happening to it right now.
var ErrRunning = errors.New(
	"a migration is running against this database; wait for it to finish")

// ErrAlreadyClosed is returned when there is nothing left to settle.
var ErrAlreadyClosed = errors.New("this request is already closed")

// settled reports the states a request can no longer be acted on from.
//
// One list, consulted by everything that acts on a request, so a new terminal
// state cannot be added and quietly forgotten by half the callers.
func settled(state string) bool {
	switch state {
	case "CLOSED", "DONE", "REVERTED":
		return true
	}
	return false
}

// running reports that statements are being applied right now, or have been.
//
// Discussion stops here rather than at the end. Once a change is on its way to
// a database, a comment cannot alter what happens — and a question asked at
// that point reads as though it might still be answered in time.
func running(state string) bool {
	switch state {
	case "EXECUTING", "COMPLETED", "FAILED", "NEEDS_ATTENTION",
		"DONE", "REVERTED", "CLOSED":
		return true
	}
	return false
}

// decidable reports the states in which a verdict still changes something.
//
// A review is advice given before a thing happens. Once a migration has been
// queued the advice has been taken; once it has run, approving it is a comment
// on the past dressed as a gate, and rejecting it does not un-run it. This is
// how a merged pull request behaves: the review controls go, the conversation
// stays.
//
// Kept next to settled() so the two lists are read together, because the
// difference between them is the whole of what closing adds.
func decidable(state string) bool {
	switch state {
	case "INITIATED", "STAGE_SANITY", "IN_REVIEW", "CHANGES_REQUESTED":
		return true
	}
	return false
}

// requireDecidable refuses a verdict on a request that has moved past review.
func (s *Scope) requireDecidable(ctx context.Context, requestID int64) error {
	var state string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT state FROM schemaver.change_request
		 WHERE id = $1 AND project_id = ANY($2)`, requestID, s.projects).
		Scan(&state); err != nil {
		return fmt.Errorf("check the request is open to review: %w", err)
	}
	if settled(state) {
		return ErrAlreadyClosed
	}
	if !decidable(state) {
		return fmt.Errorf(
			"this migration has already been queued or run, so a verdict on it "+
				"changes nothing; to stop it or settle it, close the request "+
				"(it is %s)", strings.ToLower(strings.ReplaceAll(state, "_", " ")))
	}
	return nil
}

// MarkDone ends a request whose migration ran and which nobody intends to undo.
//
// Separate from CloseRequest because they are different endings and a list
// should say which: DONE is a change that landed and is finished with, CLOSED
// is one that never ran. Both freeze the request; only the words differ, and
// the words are the point.
func (s *Scope) MarkDone(ctx context.Context, actorID, requestID int64, note string) error {
	return s.settle(ctx, actorID, requestID, "DONE", note, "done")
}

// CloseRequest settles a change request that will not run.
//
// The state a request reaches when its migration succeeds is COMPLETED, which
// says the statements ran. It does not say anybody is finished with it: the
// rollback stays on offer, because the hour after a change lands is exactly
// when somebody decides it was wrong. Closing is how they say that hour is
// over.
//
// It therefore gives up the rollback shortcut, which is the only thing closing
// actually costs and is worth saying out loud. The capability does not go away
// — the way back is to propose the reverse as its own change, which is reviewed
// like anything else — but the button on this page does.
//
// Also the way to settle a request nobody is going to run: one abandoned in
// review, or one left in NEEDS_ATTENTION after somebody sorted the database out
// by hand. Both are common and neither had an ending.
func (s *Scope) CloseRequest(ctx context.Context, actorID, requestID int64, note string) error {
	return s.settle(ctx, actorID, requestID, "CLOSED", note, "closed")
}

// settle is the ending both share.
func (s *Scope) settle(ctx context.Context, actorID, requestID int64, target, note, verb string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}

	var state string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT state FROM schemaver.change_request
		 WHERE id = $1 AND project_id = ANY($2)`, requestID, s.projects).
		Scan(&state); err != nil {
		return fmt.Errorf("load the request: %w", err)
	}
	switch {
	case settled(state):
		return ErrAlreadyClosed
	case state == "EXECUTING":
		// Closing now would leave the interface saying a request is finished
		// while statements are still being applied to a database.
		return ErrRunning
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Work queued against this request would otherwise run after it was
	// declared finished. A migration waiting to be claimed is the case that
	// matters: closing a request and then watching it execute would be the
	// worst possible reading of the word.
	tag, err := tx.Exec(ctx, `
		UPDATE schemaver.job j
		   SET state = 'cancelled', finished_at = now(),
		       error = 'the change request was closed'
		 WHERE j.state IN ('pending', 'running')
		   AND ((j.target_kind = 'request' AND j.target_id = $1)
		     OR (j.target_kind = 'migration' AND EXISTS (
		           SELECT 1 FROM schemaver.migration m
		            WHERE m.id = j.target_id AND m.change_request_id = $1)))`,
		requestID)
	if err != nil {
		return fmt.Errorf("cancel queued work: %w", err)
	}
	cancelled := int(tag.RowsAffected())

	reason := strings.TrimSpace(note)
	if reason == "" {
		reason = verb
	}
	if _, err := tx.Exec(ctx, `
		UPDATE schemaver.change_request
		   SET state = $4, state_reason = $2, closed_at = now(),
		       closed_by = $3, updated_at = now()
		 WHERE id = $1`, requestID, reason, actorID, target); err != nil {
		return fmt.Errorf("settle the request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	message := verb + ": " + reason
	if cancelled > 0 {
		message = fmt.Sprintf("%s: %s (%d queued job(s) cancelled)", verb, reason, cancelled)
	}
	s.record(ctx, Info("request."+verb, message).
		By(actorID).
		OnRequest(requestID).
		With(map[string]any{"from": state, "cancelled_jobs": cancelled}))
	return nil
}

// ErrNotDiscussable is returned when a request has moved past discussion.
var ErrNotDiscussable = errors.New(
	"this change is already being applied, or has been; the discussion is closed")

// requireDiscussable refuses a comment on a request whose change is on its way.
func (s *Scope) requireDiscussable(ctx context.Context, requestID int64) error {
	var state string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT state FROM schemaver.change_request
		 WHERE id = $1 AND project_id = ANY($2)`, requestID, s.projects).
		Scan(&state); err != nil {
		return fmt.Errorf("check the request is open to discussion: %w", err)
	}
	if running(state) {
		return ErrNotDiscussable
	}
	return nil
}

// requireOpen refuses an action on a request that has been settled.
//
// Its own method so that every path acting on a request asks the same question
// in the same words, rather than each remembering its own list of states.
func (s *Scope) requireOpen(ctx context.Context, requestID int64) error {
	var state string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT state FROM schemaver.change_request
		 WHERE id = $1 AND project_id = ANY($2)`, requestID, s.projects).
		Scan(&state); err != nil {
		return fmt.Errorf("check the request is open: %w", err)
	}
	if settled(state) {
		return ErrAlreadyClosed
	}
	return nil
}
