-- The database a change passes through before this one.
--
-- `expected_peer_id` has meant "the database this one should match", and drove
-- drift alone. It is the same edge a promotion runs along, read in the other
-- direction: production pointing at staging says staging is the truth
-- production follows, which is exactly what "changes reach staging first" says.
-- So it gets a second job rather than a second column, and a deployment has one
-- relationship to keep right instead of two that must agree.
--
-- Chains fall out of it without further machinery: production points at
-- staging, staging points at development, and each is gated on the one below.
--
-- What makes the gate cheap is that a migration already names the fingerprint
-- it is trying to reach. "Has this change been through staging" is therefore
-- "is staging already at that fingerprint" — a comparison of two values that
-- are both already stored, needing no record of which migration ran where.
--
-- D-010's expiring evidence falls out of the same comparison. A rehearsal is
-- not a fact that ages; it is true exactly while staging sits at the schema
-- production is aiming for. If staging moves, the fingerprints stop matching
-- and the gate shuts again by itself, with nothing to expire and nothing to
-- sweep.

COMMENT ON COLUMN schemaver.database.expected_peer_id IS
    'The database this one follows: the schema it is expected to match, and the '
    'environment a change passes through before reaching here. Drift compares '
    'against it; the execute gate requires it to have reached a migration''s '
    'target before that migration may run here. Null means nothing precedes '
    'this database, so neither check applies.';

-- Ordering exists on environments and has never been read for anything but
-- sorting a dropdown. It is what tells a promotion link pointing the wrong way
-- from one pointing the right way, so it is worth an index now that something
-- consults it.
CREATE INDEX IF NOT EXISTS environment_rank ON schemaver.environment (project_id, rank);
