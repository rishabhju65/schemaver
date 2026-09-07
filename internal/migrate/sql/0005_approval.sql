-- Approval: a project administrator must approve before anything executes.

-- The role the reviewer held when they decided.
--
-- Recorded rather than joined at read time, deliberately. Re-evaluating history
-- against current membership would make "was this approved?" answer differently
-- as people change teams, and an approval is an account of something that
-- happened, not a standing claim. It also keeps the audit truthful when someone
-- leaves.
ALTER TABLE schemaver.review_decision
    ADD COLUMN reviewer_role text NOT NULL DEFAULT 'viewer'
        CHECK (reviewer_role IN ('admin', 'operator', 'viewer')),

    -- True when the approver is also the author. Permitted only where they are
    -- the project's sole administrator — otherwise there is nobody else to ask —
    -- and always visible, because an unreviewed change that looks reviewed is
    -- worse than one that admits it.
    ADD COLUMN self_approved boolean NOT NULL DEFAULT false;

CREATE INDEX review_decision_approvals_idx
    ON schemaver.review_decision (migration_id, decision)
    WHERE decision = 'approve';
