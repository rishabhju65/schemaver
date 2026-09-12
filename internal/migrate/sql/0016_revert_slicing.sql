-- Which forward statement each revert statement undoes.
--
-- Without this a revert is all-or-nothing, and all-or-nothing is exactly wrong
-- for the case it is most needed. A migration that stops halfway leaves the
-- database at neither its start nor its target, and the generated revert —
-- which assumes the target was reached — does not apply to it.
--
-- With it, a rollback from a half-applied migration is the subset of revert
-- statements covering the forward statements that actually ran. That subset is
-- made only of statements a reviewer already read and approved, which is the
-- property that makes it usable during an incident: nothing new is composed at
-- the worst possible moment.
--
-- The mapping is by object, not by statement text. A forward change and its
-- inverse have different ids by design — drop_column:x is undone by
-- add_column:x — and what they share is the thing they are about.

ALTER TABLE schemaver.migration_revert_step
    ADD COLUMN undoes_ordinal integer;

COMMENT ON COLUMN schemaver.migration_revert_step.undoes_ordinal IS
    'The forward statement this one reverses. Null where no forward statement '
    'could be matched, in which case the revert cannot be sliced and only a '
    'full rollback from the target is offered.';
