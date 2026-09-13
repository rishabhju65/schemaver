-- Which way a run went, and where a request ends up.
--
-- Three problems, all of them the same one: the page could not say what had
-- happened.
--
-- A rollback produced an execution row indistinguishable from the migration's
-- own. The timeline said a revert had run and the execution panel showed a run
-- with different statements and no explanation, so a reader had to infer from
-- the SQL which direction it went — on the page they are reading precisely
-- because something went wrong.
--
-- A rolled-back request stayed COMPLETED, which is the opposite of true. The
-- statements had been undone and the page reported success.
--
-- And COMPLETED meant two things at once: the migration ran, and nobody has
-- finished with this. Those want separating, because the second is a decision
-- and the first is a fact.

ALTER TABLE schemaver.execution
    ADD COLUMN direction text NOT NULL DEFAULT 'forward'
        CHECK (direction IN ('forward', 'revert'));

COMMENT ON COLUMN schemaver.execution.direction IS
    'Whether this run applied the migration or undid it. Both are executions of '
    'the same migration against the same database, and the statements alone do '
    'not say which — a reader should not have to work it out from the SQL.';

ALTER TABLE schemaver.change_request DROP CONSTRAINT change_request_state_check;

ALTER TABLE schemaver.change_request ADD CONSTRAINT change_request_state_check
    CHECK (state IN (
        -- Under way.
        'INITIATED', 'STAGE_SANITY', 'IN_REVIEW', 'CHANGES_REQUESTED',
        'READY_TO_EXECUTE', 'EXECUTING',
        -- Ran, and still open: the rollback is on offer and somebody may yet
        -- decide the change was wrong.
        'COMPLETED',
        -- Wants a person.
        'FAILED', 'NEEDS_ATTENTION', 'STALE',
        -- Ended. DONE is somebody saying they are finished with a change that
        -- ran; REVERTED is that change having been undone; CLOSED is a request
        -- that never ran and will not. Three endings because they are three
        -- different things to have happened, and a reader glancing at a list
        -- should not have to open one to find out which.
        'DONE', 'REVERTED', 'CLOSED'));
