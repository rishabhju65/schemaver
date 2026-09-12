-- The way back, generated alongside the way forward.
--
-- D-012 has said since it was written that a revert is generated and read at
-- approval time. Nothing generated one. The only part that existed was
-- `irreversible_reason`, which names the objects a migration discards — and on
-- its own that column is worse than nothing, because a reviewer seeing it
-- absent reasonably concludes a way back exists, when none was ever written
-- down for any change.
--
-- Kept in its own table rather than as more rows in `migration_step`, and that
-- is deliberate. D-012's scope cut for this version says nothing runs a revert:
-- there is no button, no job, no precondition and no proof behind one. A shape
-- the executor cannot accidentally pick up is the structural way to hold that
-- line, rather than a `direction` column on the table it already drains.

CREATE TABLE schemaver.migration_revert_step (
    migration_id bigint NOT NULL
        REFERENCES schemaver.migration (id) ON DELETE CASCADE,
    ordinal integer NOT NULL,

    sql       text NOT NULL,
    change_id text NOT NULL,

    transactional boolean NOT NULL DEFAULT true,
    note text,

    -- This statement puts the shape back and cannot put the contents back.
    --
    -- The single most important thing this table can tell a reviewer. The
    -- revert of `DROP COLUMN legacy_status` is `ADD COLUMN legacy_status text`,
    -- which is a perfectly good statement that restores an empty column. Read
    -- without this flag, a generated revert reads as an undo, and the moment to
    -- discover it is not one is before the forward migration runs, not after.
    structure_only boolean NOT NULL DEFAULT false,

    PRIMARY KEY (migration_id, ordinal)
);

-- No expected_after here, unlike the forward steps. Those fingerprints come
-- from D-021's proof, and the revert is not proven: it is derived by the same
-- engine from the same pair of schemas read backwards, so it is as good as the
-- diff engine and no better. Recording a fingerprint chain for it would imply a
-- check that never ran, which is the mistake D-021 exists to have corrected.
