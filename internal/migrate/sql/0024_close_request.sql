-- When a change request was settled, and by whom.
--
-- COMPLETED says the statements ran. It does not say anybody is finished: the
-- rollback stays on offer afterwards, because the hour after a change lands is
-- exactly when somebody decides it was wrong. Nothing said that hour was over,
-- so a request that succeeded months ago still offered to undo itself, and a
-- request abandoned in review or left in NEEDS_ATTENTION after somebody fixed
-- the database by hand had no ending at all.
--
-- CLOSED already existed as a state, reached by rejecting a request or by
-- retiring the database under it. This gives it a deliberate entrance and
-- records who used it.

ALTER TABLE schemaver.change_request
    ADD COLUMN closed_at timestamptz,
    ADD COLUMN closed_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL;
