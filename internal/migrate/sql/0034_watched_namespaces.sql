-- Which schemas inside a database this one is watching.
--
-- A database is the repository and its schemas are directories in it. Until now
-- every directory was in, whether anybody wanted it or not: a fingerprint
-- covered every non-system namespace, so a change in one team's schema was
-- drift on the whole database, and a branch cut to touch one carried all of
-- them — which means a merge could raise a conflict in a namespace its author
-- has no authority over and never touched.
--
-- Exclusion rather than inclusion, and the direction is the decision. An
-- include-list is a closed world that has to be re-enumerated whenever a schema
-- appears, and schemas appear at runtime: a tenant is onboarded, an extension
-- is installed. Everything that is not listed is then silently unwatched, which
-- is the failure this product exists to prevent — a report of "nothing changed"
-- that is really "nothing was looked at". An exclude-list is an open world: a
-- new schema shows up as a difference, which is noise somebody can silence,
-- rather than silence somebody has to notice.
--
-- Empty by default, so a database watches everything until somebody says
-- otherwise. That is what every database does today, so nothing changes until
-- it is chosen.
ALTER TABLE schemaver.database
    ADD COLUMN excluded_namespaces text[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN schemaver.database.excluded_namespaces IS
    'Schemas inside this database that are not watched: not fingerprinted, not '
    'compared for drift, not carried by a branch cut from it. Excluding one '
    'changes the database''s fingerprint, because it changes what the '
    'fingerprint is of.';
