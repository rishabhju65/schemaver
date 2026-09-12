-- REVIEWED leaves the state machine: eleven states, not twelve.
--
-- It was in the CHECK constraint from the day the review tables were written
-- and nothing ever set it. That is not an oversight waiting to be corrected —
-- it could not honestly be set at all.
--
-- Whether a migration is approved is computed from its review_decision rows
-- every time anybody asks, against the fingerprint pair those decisions were
-- made about. A stored REVIEWED would be a second copy of that answer, and the
-- two would disagree the moment a regeneration silently withdrew the approvals.
-- That withdrawal is the case the derived answer exists to get right, so the
-- stored one would be wrong in exactly the situation that matters.
--
-- D-021 also reversed the order this state sat in. The happy path now runs
-- INITIATED → STAGE_SANITY → IN_REVIEW → READY_TO_EXECUTE: the shadow proof
-- comes before review rather than after it, because asking somebody to read a
-- migration that has not been shown to apply is asking them to approve a guess.
-- There is no longer a point in the flow where "reviewed but not yet proven"
-- describes anything.

ALTER TABLE schemaver.change_request DROP CONSTRAINT change_request_state_check;

ALTER TABLE schemaver.change_request ADD CONSTRAINT change_request_state_check
    CHECK (state IN (
        'INITIATED', 'STAGE_SANITY', 'IN_REVIEW',
        'READY_TO_EXECUTE', 'EXECUTING', 'COMPLETED',
        'CHANGES_REQUESTED', 'STALE', 'FAILED', 'NEEDS_ATTENTION', 'CLOSED'));
