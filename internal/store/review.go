package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Decision is one reviewer's verdict on a migration.
type Decision struct {
	ReviewerID    int64
	ReviewerLabel string
	ReviewerRole  auth.Role
	Verdict       string // approve, request_changes, reject
	Comment       string
	SelfApproved  bool
	DecidedAt     time.Time
}

// Approves reports whether this decision is an approval by a project
// administrator, which is the only kind that satisfies the gate.
func (d Decision) Approves() bool {
	return d.Verdict == "approve" && d.ReviewerRole == auth.Admin
}

// Blocks reports whether this decision prevents execution outright.
func (d Decision) Blocks() bool {
	return d.Verdict == "reject" || d.Verdict == "request_changes"
}

// ApprovalState is everything the execute gate depends on.
//
// Computed rather than stored. A stored "approved" flag would have to be
// invalidated whenever the migration is regenerated, a reviewer changes their
// mind, or a rename question appears — and every one of those is a chance to
// leave a stale flag that says a change is ready when it is not.
type ApprovalState struct {
	MigrationID int64
	// FromFingerprint and ToFingerprint identify which migration this state
	// describes. Decisions about any other pair are not counted.
	FromFingerprint string
	ToFingerprint   string

	Decisions []Decision

	AdminApprovals int
	Blocking       int
	// SoleAdmin reports that the author is the project's only administrator, in
	// which case their own approval is the only one obtainable.
	SoleAdmin bool
	// UnansweredRenames counts rename proposals nobody has resolved. A rename
	// and a drop-plus-add are indistinguishable from schema alone, so executing
	// with one outstanding risks destroying a column.
	UnansweredRenames int

	// ProofState is whether this migration has been applied to a throwaway
	// database and checked against its declared target: pending, passed, failed
	// or unproven. ProofReason says why, when it did not pass.
	ProofState  string
	ProofReason string

	Executable bool
	// Reason explains a refusal, in the terms the person reading it can act on.
	Reason string
}

// ErrNoMigration is returned for a request that has not generated one yet.
var ErrNoMigration = errors.New("this request has no generated migration")

// ApprovalState evaluates the execute gate for a change request.
func (s *Scope) ApprovalState(ctx context.Context, requestID int64) (*ApprovalState, error) {
	var (
		st        ApprovalState
		authorID  *int64
		projectID int64
		renames   int
	)

	err := s.store.pool.QueryRow(ctx, `
		SELECT m.id, m.from_fingerprint, m.to_fingerprint,
		       -- Tolerate a scalar null from any row written before the
		       -- generator was fixed to store an empty array.
		       jsonb_array_length(
		           COALESCE(NULLIF(m.rename_candidates, 'null'::jsonb), '[]'::jsonb)),
		       r.author_id, r.project_id,
		       m.proof_state, COALESCE(m.proof_reason, '')
		  FROM schemaver.change_request r
		  JOIN schemaver.migration m ON m.change_request_id = r.id
		 WHERE r.id = $1 AND r.project_id = ANY($2) AND m.superseded_at IS NULL
		 ORDER BY m.generated_at DESC
		 LIMIT 1`, requestID, s.projects).
		Scan(&st.MigrationID, &st.FromFingerprint, &st.ToFingerprint,
			&renames, &authorID, &projectID, &st.ProofState, &st.ProofReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoMigration
	}
	if err != nil {
		return nil, fmt.Errorf("load migration for request %d: %w", requestID, err)
	}
	st.UnansweredRenames = renames

	// Only decisions about *this* migration count. A regenerated migration
	// leaves its predecessor's approvals behind without anything having to
	// withdraw them.
	rows, err := s.store.pool.Query(ctx, `
		SELECT COALESCE(reviewer_id, 0), reviewer_label, reviewer_role,
		       decision, COALESCE(comment, ''), self_approved, decided_at
		  FROM schemaver.review_decision
		 WHERE migration_id = $1
		 ORDER BY decided_at`, st.MigrationID)
	if err != nil {
		return nil, fmt.Errorf("load decisions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var d Decision
		var role string
		if err := rows.Scan(&d.ReviewerID, &d.ReviewerLabel, &role, &d.Verdict,
			&d.Comment, &d.SelfApproved, &d.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		d.ReviewerRole = auth.Role(role)
		st.Decisions = append(st.Decisions, d)
		if d.Approves() {
			st.AdminApprovals++
		}
		if d.Blocks() {
			st.Blocking++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if authorID != nil {
		st.SoleAdmin, err = s.store.soleAdmin(ctx, projectID, *authorID)
		if err != nil {
			return nil, err
		}
	}

	st.Executable, st.Reason = st.evaluate()
	return &st, nil
}

// evaluate applies the gate, in the order a reader would ask the questions.
func (st *ApprovalState) evaluate() (bool, string) {
	switch {
	case st.ProofState == "pending":
		return false, "this migration has not finished being proven against a " +
			"throwaway copy of the database yet"
	case st.ProofState == "failed":
		return false, "this migration did not produce the schema it declares when " +
			"applied to a throwaway copy: " + st.ProofReason
	case st.Blocking > 0:
		return false, "a reviewer has requested changes or rejected this migration"
	case st.UnansweredRenames > 0:
		return false, fmt.Sprintf(
			"%d rename question(s) are unanswered; a rename and a drop-and-add are "+
				"indistinguishable from the schema alone, and guessing destroys data",
			st.UnansweredRenames)
	case st.AdminApprovals == 0:
		return false, "no project administrator has approved this migration"
	}
	return true, ""
}

// soleAdmin reports whether a user is the only administrator of a project.
func (s *Store) soleAdmin(ctx context.Context, projectID, userID int64) (bool, error) {
	var others int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM schemaver.project_member
		 WHERE project_id = $1 AND role = 'admin' AND user_id <> $2`,
		projectID, userID).Scan(&others); err != nil {
		return false, fmt.Errorf("count administrators: %w", err)
	}
	return others == 0, nil
}

// ErrSelfApproval is returned when an author approves their own request and is
// not the project's only administrator.
var ErrSelfApproval = errors.New(
	"you cannot approve your own request; another project administrator must review it")

// ErrNotAdmin is returned when a non-administrator attempts to approve.
var ErrNotAdmin = errors.New("only a project administrator can approve a migration")

// Decide records a reviewer's verdict on the current migration.
//
// The reviewer's role is captured here rather than looked up later, so the
// record says what was true when the decision was made.
func (s *Scope) Decide(ctx context.Context, requestID, reviewerID int64, verdict, comment string) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	switch verdict {
	case "approve", "request_changes", "reject":
	default:
		return fmt.Errorf("unknown verdict %q", verdict)
	}

	var migrationID, projectID int64
	var from, to string
	var authorID *int64
	err := s.store.pool.QueryRow(ctx, `
		SELECT m.id, m.from_fingerprint, m.to_fingerprint, r.project_id, r.author_id
		  FROM schemaver.change_request r
		  JOIN schemaver.migration m ON m.change_request_id = r.id
		 WHERE r.id = $1 AND r.project_id = ANY($2) AND m.superseded_at IS NULL
		 ORDER BY m.generated_at DESC LIMIT 1`, requestID, s.projects).
		Scan(&migrationID, &from, &to, &projectID, &authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoMigration
	}
	if err != nil {
		return fmt.Errorf("load migration: %w", err)
	}

	role, member, err := s.store.MemberOf(ctx, reviewerID, projectID)
	if err != nil {
		return err
	}
	if !member {
		return fmt.Errorf("you are not a member of this project")
	}
	if verdict == "approve" && role != auth.Admin {
		return ErrNotAdmin
	}

	self := authorID != nil && *authorID == reviewerID
	if self && verdict == "approve" {
		sole, err := s.store.soleAdmin(ctx, projectID, reviewerID)
		if err != nil {
			return err
		}
		if !sole {
			return ErrSelfApproval
		}
	}

	var label string
	if err := s.store.pool.QueryRow(ctx,
		`SELECT email FROM schemaver.app_user WHERE id = $1`, reviewerID).Scan(&label); err != nil {
		return fmt.Errorf("load reviewer: %w", err)
	}

	// One standing decision per reviewer: changing your mind replaces it rather
	// than accumulating opinions nobody knows how to count.
	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO schemaver.review_decision
		    (change_request_id, migration_id, reviewer_id, reviewer_label,
		     reviewer_role, decision, comment, self_approved,
		     from_fingerprint, to_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10)
		ON CONFLICT (migration_id, reviewer_id) DO UPDATE
		   SET decision = EXCLUDED.decision,
		       comment = EXCLUDED.comment,
		       reviewer_role = EXCLUDED.reviewer_role,
		       self_approved = EXCLUDED.self_approved,
		       decided_at = now()`,
		requestID, migrationID, reviewerID, label, string(role), verdict,
		comment, self, from, to); err != nil {
		return fmt.Errorf("record decision: %w", err)
	}

	// Requesting changes moves the request back to the author; approving does
	// not advance it, because the other gates have their own say.
	if verdict == "request_changes" || verdict == "reject" {
		next, reason := "CHANGES_REQUESTED", "a reviewer requested changes"
		if verdict == "reject" {
			next, reason = "CLOSED", "rejected in review"
		}
		if _, err := s.store.pool.Exec(ctx, `
			UPDATE schemaver.change_request
			   SET state = $2, state_reason = $3, updated_at = now()
			 WHERE id = $1`, requestID, next, reason); err != nil {
			return fmt.Errorf("update request state: %w", err)
		}
	}

	// A decision names the fingerprint pair it was made against, and so does
	// this entry. An approval silently withdrawn by regeneration is otherwise
	// invisible in hindsight: the timeline would show an approval and then an
	// unexplained shut gate.
	verdicts := map[string]string{
		"approve":         "approved",
		"request_changes": "requested changes on",
		"reject":          "rejected",
	}
	said, ok := verdicts[verdict]
	if !ok {
		said = verdict
	}
	entry := Info("review."+verdict, fmt.Sprintf("%s as %s, for %s → %s",
		said, role, schema.Version(from).Short(), schema.Version(to).Short()))
	if verdict != "approve" {
		entry = Warn("review."+verdict, entry.Message)
	}
	s.record(ctx, entry.
		By(reviewerID).
		OnRequest(requestID).
		OnMigration(migrationID).
		With(map[string]any{
			"verdict": verdict, "role": string(role), "self_approved": self,
			"from": from, "to": to,
		}))
	return nil
}
