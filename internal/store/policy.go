package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/auth"
)

// Policy is how much a project asks for between a change and a database.
//
// Only the parts that are genuinely a choice. A migration that failed its
// rehearsal, a reviewer who has objected and an unanswered rename question are
// not settings — they are the change being wrong, somebody saying so, and a
// column's data waiting on an answer.
type Policy struct {
	// ApprovalsRequired is how many project administrators must approve. Zero
	// means none: a project whose only administrator is also its only author
	// gains nothing from approving their own work.
	ApprovalsRequired int

	// RevertRequired is whether somebody must write the way back, or say why
	// there is not one, before a change can run.
	//
	// Turning it off does not leave a revert unchecked. One written anyway is
	// still rehearsed, and one that does not lead back still blocks. What is
	// given up is being asked.
	RevertRequired bool

	UpdatedBy string
}

// DefaultPolicy is what a project has until somebody changes it, and is exactly
// what every project had before the policy existed.
func DefaultPolicy() Policy {
	return Policy{ApprovalsRequired: 1, RevertRequired: true}
}

// Policy reads this project's settings.
func (s *Scope) Policy(ctx context.Context) (Policy, error) {
	p := DefaultPolicy()
	err := s.store.pool.QueryRow(ctx, `
		SELECT pp.approvals_required, pp.revert_required, COALESCE(u.email, '')
		  FROM schemaver.project_policy pp
		  LEFT JOIN schemaver.app_user u ON u.id = pp.updated_by
		 WHERE pp.project_id = ANY($1)
		 LIMIT 1`, s.projects).
		Scan(&p.ApprovalsRequired, &p.RevertRequired, &p.UpdatedBy)
	if err != nil {
		// No row is the ordinary case and means the defaults.
		return DefaultPolicy(), nil
	}
	return p, nil
}

// SetPolicy records what this project asks for.
func (s *Scope) SetPolicy(ctx context.Context, actorID int64, p Policy) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	if p.ApprovalsRequired < 0 || p.ApprovalsRequired > 5 {
		return errors.New("a project can ask for between none and five approvals")
	}

	// Administrators only, checked here rather than in a handler. This setting
	// decides how much review the project's own changes get, so anybody who
	// could change it could also exempt themselves from it.
	role, member, err := s.store.MemberOf(ctx, actorID, s.writable)
	if err != nil {
		return err
	}
	if !member || role != auth.Admin {
		return errors.New("only a project administrator can change what the " +
			"project asks for before a change runs")
	}

	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO schemaver.project_policy
		    (project_id, approvals_required, revert_required, updated_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id) DO UPDATE
		   SET approvals_required = EXCLUDED.approvals_required,
		       revert_required = EXCLUDED.revert_required,
		       updated_by = EXCLUDED.updated_by, updated_at = now()`,
		s.writable, p.ApprovalsRequired, p.RevertRequired, actorID); err != nil {
		return fmt.Errorf("record the policy: %w", err)
	}

	// Recorded because loosening a gate is exactly the kind of change somebody
	// later wants to know the date of.
	s.record(ctx, Info("project.policy_changed", describePolicy(p)).By(actorID))
	return nil
}

func describePolicy(p Policy) string {
	approvals := fmt.Sprintf("%d approval(s)", p.ApprovalsRequired)
	if p.ApprovalsRequired == 0 {
		approvals = "no approval"
	}
	way := "a way back must be written"
	if !p.RevertRequired {
		way = "a way back is not required"
	}
	return "changes now need " + approvals + "; " + way
}
