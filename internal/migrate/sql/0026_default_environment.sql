-- A discovered database is production until somebody says otherwise.
--
-- The environment was left unset and stayed unset unless somebody went to the
-- server's page and chose one. Since D-024 that label decides something: it
-- tells a promotion link pointing the right way from one pointing backwards,
-- and an unlabelled database makes that check permissive because it cannot
-- compare ranks that are not there. So the common case — nobody has labelled
-- anything — quietly disabled a safeguard.
--
-- Defaulting to the *most* guarded environment is the only defensible
-- direction. Assume a database is production and the worst a mislabelling
-- causes is ceremony somebody removes; assume it is staging and the worst is a
-- change reaching production without passing through anything, which is the
-- arrangement D-024 exists to prevent. An unlabelled database is unknown, and
-- unknown should be treated as the dangerous case.
--
-- "Production" is taken as the highest-ranked environment rather than the one
-- named production, because rank is what the checks read and a name can be
-- changed. A project that renames its environments keeps the meaning.

-- Every project needs the two, including ones created by the seed file rather
-- than by sign-up, which had none at all — so their databases could not be
-- labelled and the direction check never ran.
INSERT INTO schemaver.environment (name, rank, project_id)
SELECT v.name, v.rank, p.id
  FROM schemaver.project p
 CROSS JOIN (VALUES ('staging', 10), ('production', 20)) AS v(name, rank)
 WHERE NOT EXISTS (
       SELECT 1 FROM schemaver.environment e
        WHERE e.project_id = p.id AND e.name = v.name);

-- Databases discovered before this, which have no label at all.
UPDATE schemaver.database d
   SET environment_id = (
       SELECT e.id FROM schemaver.environment e
         JOIN schemaver.instance i ON i.id = d.instance_id
        WHERE e.project_id = i.project_id
        ORDER BY e.rank DESC
        LIMIT 1)
 WHERE d.environment_id IS NULL;
