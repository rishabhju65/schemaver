-- A way back, or a statement that there is none. One of the two.
--
-- Writing the revert was compulsory. Almost nothing else in this field requires
-- that — Liquibase derives one where it can and lets raw SQL go without,
-- Flyway's undo is a paid extra the community edition lacks entirely, Rails and
-- Django both let a migration be irreversible and say so only when somebody
-- tries. The shared pattern is: derive it where possible, allow absence, fail
-- at rollback time.
--
-- They are right for a reason worth stating. Faced with a compulsory box,
-- somebody with a DROP COLUMN to ship writes "-- nothing to do" or pastes an
-- ADD COLUMN that restores an empty column, and the result is worse than an
-- honest absence: it looks like a way back. A mandatory field buys compliance,
-- not reversibility.
--
-- But D-012's purpose was never the script. It was that "irreversibility is
-- surfaced at approval time, when a human can still choose differently — never
-- discovered during the incident". That is about forced consideration, and it
-- survives without the mandate: require the author to decide, and let one of
-- the answers be "this cannot be undone, because...".
--
-- So the script becomes optional and the decision does not. A migration with
-- neither is refused, as it was before; a migration with a stated reason is
-- approved knowing there is no way back, which is exactly the choice D-012
-- wanted somebody to make while they still could.

ALTER TABLE schemaver.migration
    -- Why this change cannot be undone, in the author's words. Distinct from
    -- irreversible_reason, which the diff engine derives by classifying changes
    -- as destructive: that one says what the engine noticed, this one says what
    -- a person decided. They usually agree, and when they do not it is the
    -- person who is answerable.
    ADD COLUMN no_revert_reason text,
    ADD COLUMN no_revert_by bigint
        REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    ADD COLUMN no_revert_at timestamptz,

    -- Both at once is a contradiction: a way back that somebody has also
    -- declared impossible. Enforced here rather than in the application,
    -- because it is the kind of state that arrives through a path nobody
    -- thought about.
    ADD CONSTRAINT migration_revert_is_written_or_declined CHECK (
        revert_authored_at IS NULL OR no_revert_reason IS NULL);
