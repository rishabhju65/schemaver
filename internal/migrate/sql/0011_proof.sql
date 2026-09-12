-- Whether a migration has been proven, and where each statement would leave the
-- database.
--
-- D-009 says a migration is applied to a throwaway database built at its
-- starting point and the result checked against its declared target, before any
-- real database is touched. The machinery for that has existed since early on
-- and nothing ever called it: no configuration named a server to build shadows
-- on, and no code path asked for one. The proof was a library, not a check.
--
-- Worse, the executor's most serious message — the one shown when every
-- statement succeeded and the schema still came out wrong — told the reader
-- "the shadow proof passed for this migration, so either something changed
-- outside schemaver while it ran, or an assumption is wrong". That sentence was
-- false, and it was read at the moment it would do the most damage.
--
-- Proof is a property of the migration rather than a state of the request, for
-- the same reason approval is: it is made against one fingerprint pair, and
-- regenerating the migration makes a new row whose proof has not been done yet.
-- Nothing has to expire it.

ALTER TABLE schemaver.migration
    ADD COLUMN proof_state text NOT NULL DEFAULT 'pending'
        CHECK (proof_state IN (
            -- Queued, or running right now.
            'pending',
            -- Applied to a shadow and produced the declared target.
            'passed',
            -- Applied to a shadow and did not.
            'failed',
            -- No shadow server is configured, so nothing was attempted. Held
            -- apart from 'failed' because a deployment that has not set this up
            -- has not discovered a bad migration, and saying so would be the
            -- same kind of lie the executor was telling.
            'unproven')),
    ADD COLUMN proof_reason text,
    ADD COLUMN proved_at    timestamptz;

-- The fingerprint the schema is expected to hold once this statement has
-- applied. Filled by the proof, one per statement.
--
-- The column was declared in 0001 and left null, with a comment saying the
-- chain needed each step simulated in a shadow database. This is that.
COMMENT ON COLUMN schemaver.migration_step.expected_after IS
    'Fingerprint after this statement, established by the shadow proof. Lets a '
    'half-applied migration be placed exactly: the database is at the value '
    'recorded for statement four, so statements one to four applied and the '
    'rest did not.';

-- Migrations generated before proofs ran were never attempted, and must not be
-- reported as having passed one.
UPDATE schemaver.migration SET proof_state = 'unproven' WHERE superseded_at IS NULL;
