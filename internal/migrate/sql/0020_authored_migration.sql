-- Writing a change, rather than copying one.
--
-- Until now every change request said "make this database look like that one".
-- That is a propagation mechanism, and it cannot express the most ordinary
-- schema change there is: a table that does not exist anywhere yet. Somebody
-- wanting a new table had to create it by hand in one database first, outside
-- schemaver and unreviewed, so that schemaver could then copy it — which means
-- the first application of every change escaped the review this product exists
-- to provide.
--
-- D-001 always intended both halves. The declared desired state lived in a
-- repository under D-004, and D-018 removed the repository without giving the
-- declaration anywhere else to live, so `declared_schema` has stood empty and
-- peer comparison has been the only way in.
--
-- What makes authored SQL safe here is the same thing that makes a generated
-- migration safe: the shadow. Statements are applied to a throwaway database
-- built at the target's current schema, and what comes out the other side *is*
-- the target fingerprint — derived by running them, never by trusting them. The
-- diff between the two ends then classifies what changed, so a written
-- migration arrives in review with its destructive statements marked exactly as
-- a generated one does.

ALTER TABLE schemaver.change_request
    -- The SQL as somebody wrote it, kept verbatim. Statements are split from
    -- this and stored individually like any other migration's, but the original
    -- text is what they will edit, and reassembling it from the pieces would
    -- lose their formatting and comments.
    ADD COLUMN authored_sql text;

COMMENT ON COLUMN schemaver.change_request.source_database_id IS
    'The database this one is being brought in line with, or null when the '
    'change was written rather than copied. Already nullable before authoring '
    'existed; now it means something.';
