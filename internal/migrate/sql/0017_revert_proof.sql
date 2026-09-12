-- Whether the way back leads back.
--
-- D-021 proves the forward migration by applying it to a throwaway database
-- built at its starting point. With a revert that will actually be executed,
-- the same standard has to apply to it: a rollback nobody has checked is
-- exactly as dangerous as a migration nobody has checked, and it is run under
-- worse conditions — during an incident, by somebody who has just watched
-- something fail.
--
-- Checked as a round trip rather than on its own. The forward migration has
-- just been applied to the shadow, so the database is sitting precisely where a
-- real one would be when somebody asks to undo it. That is the only state the
-- revert is for, and proving it anywhere else would prove something else.
--
-- Held apart from the forward proof because a failure says something different.
-- The forward statements have already applied and verified by the time this can
-- fail, so the migration is correct and its revert is not — and recording that
-- as a failed migration would send somebody to change the half that works.

ALTER TABLE schemaver.migration
    ADD COLUMN revert_proof_state text NOT NULL DEFAULT 'pending'
        CHECK (revert_proof_state IN ('pending', 'passed', 'failed', 'unproven')),
    ADD COLUMN revert_proof_reason text;

-- Reverts generated before this existed were never checked.
UPDATE schemaver.migration SET revert_proof_state = 'unproven'
 WHERE superseded_at IS NULL;
