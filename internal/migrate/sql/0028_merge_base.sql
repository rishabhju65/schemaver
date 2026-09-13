-- Where the two sides last agreed, recorded on the migration that reconciled
-- them.
--
-- Until now a change request compared a target database against a source and
-- generated whatever would make the first look like the second. That is the
-- right answer only when the target has not moved on its own. When both have,
-- the comparison undoes the target's own work: a column production grew and
-- staging never had comes out as a DROP, classified destructive, and offered
-- for approval as though discarding it were somebody's intention. The product
-- was built to branch, diff and merge schemas, and it had the diff.
--
-- A merge needs a third point — the most recent schema both databases have been
-- observed at — recovered from the snapshots each has left behind. With it,
-- each side's independent work is the difference from there, and the two can be
-- reconciled rather than one imposed on the other.
--
-- Nullable, and null is the ordinary case: a request against a database that
-- has not diverged is a plain comparison and always was. Filled only when the
-- migration is genuinely a merge, which is exactly when a reviewer needs to
-- know, because the statements will be fewer than the difference between the
-- two schemas and nothing else on the page would explain why.
ALTER TABLE schemaver.migration
    ADD COLUMN merge_base text
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT;

COMMENT ON COLUMN schemaver.migration.to_fingerprint IS
    'The schema this migration declares it will produce, and what the shadow '
    'proof checks the rehearsal against. For a merge this is neither side''s '
    'current schema but the merged result — applying both sides'' work lands '
    'on a schema no database has been observed at, which is why it is stored '
    'as a blob when the merge is generated rather than found among the '
    'snapshots.';
