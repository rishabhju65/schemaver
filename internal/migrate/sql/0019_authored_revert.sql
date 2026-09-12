-- The way back is written, not derived.
--
-- Generating it was defensible and it worked: read the same two schemas in the
-- other order and the engine produces the inverse, correctly ordered, with the
-- statements that restore shape without contents marked. What it could not do
-- is know anything the schemas do not contain. A backfill has no inverse in a
-- diff, so a migration that filled a column was silently un-undoable, and
-- nothing on the page said so.
--
-- An authored revert is honest about that by construction: whoever wrote the
-- forward change writes the way back, including whatever the data needs, and a
-- reviewer reads both. It is also less machinery — this file removes the
-- mapping that let a revert be cut down to the part of a migration that ran.
--
-- What is lost with it: a half-applied migration can no longer be rolled back
-- automatically. The generated revert could be sliced because the system knew
-- drop_column:x was undone by add_column:x; a written one is opaque, so it can
-- only be run from the state it was written for. A migration that stops part
-- way is NEEDS_ATTENTION for a person to resolve, which is where D-013 always
-- said it belonged.

ALTER TABLE schemaver.migration_revert_step
    DROP COLUMN undoes_ordinal,
    -- Nothing derives these any more, so nothing can say which of them restore
    -- shape without contents. The author says it in a note, or the reviewer
    -- notices; neither is a claim the system is making.
    DROP COLUMN structure_only;

-- Whether anybody has written one yet. Held on the migration rather than
-- inferred from the absence of steps, because "no revert written" and "a revert
-- that happens to be empty" are different states and only one of them should
-- hold up a review.
ALTER TABLE schemaver.migration
    ADD COLUMN revert_authored_at timestamptz,
    ADD COLUMN revert_author_id bigint
        REFERENCES schemaver.app_user (id) ON DELETE SET NULL;

-- Reverts already generated stay as they are and count as authored: they were
-- reviewed as part of the migrations they belong to, and voiding them would
-- reopen approvals over a change in how they were produced rather than in what
-- they say.
UPDATE schemaver.migration m
   SET revert_authored_at = m.generated_at
 WHERE EXISTS (SELECT 1 FROM schemaver.migration_revert_step s
                WHERE s.migration_id = m.id);
