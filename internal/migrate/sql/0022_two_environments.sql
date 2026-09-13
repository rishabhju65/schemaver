-- Two environments, not three.
--
-- A project was seeded with development, staging and production. The middle one
-- is the only one the product does anything with: D-024 gates a change on the
-- environment below the one it targets, and for that a chain of two is the
-- whole idea — staging precedes production. A third rung adds a place to put a
-- database and nothing else, and every extra concept in a tool people use
-- occasionally is one more thing to work out before they can use it.
--
-- Nothing is lost by dropping it. Ranks are read only against each other —
-- ORDER BY for the list, and a comparison when checking a promotion link points
-- the right way — so renumbering preserves every meaning they carry.
--
-- Deliberately conservative about deletion. An environment nothing is assigned
-- to is scaffolding this migration put there; one a database is actually
-- labelled with is somebody's, and removing it would silently blank that label
-- because the reference is ON DELETE SET NULL. So only the unused ones go, and
-- a deployment that has put databases in development keeps it, with staging and
-- production renumbered around it.

DELETE FROM schemaver.environment e
 WHERE e.name = 'development'
   AND NOT EXISTS (SELECT 1 FROM schemaver.database d WHERE d.environment_id = e.id);

-- Renumbered so a fresh project and an upgraded one agree. Existing order is
-- preserved either way: where development survives it keeps rank 10 and sorts
-- first, which is where it belongs.
UPDATE schemaver.environment SET rank = 10 WHERE name = 'staging';
UPDATE schemaver.environment SET rank = 20 WHERE name = 'production';
