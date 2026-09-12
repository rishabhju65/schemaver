-- What an approval is an approval *of*.
--
-- Until now a decision was tied to a migration id and the fingerprint pair it
-- spans, and that was enough because the statements between those two points
-- could not change: regenerating produced a new migration row, leaving the old
-- approvals behind on a row nobody would execute. Evidence expired because the
-- thing it described was replaced wholesale.
--
-- Editing breaks that. An administrator adjusting a statement changes neither
-- the migration id nor either fingerprint, so every approval would survive — an
-- `ALTER TABLE` could be edited into a `DROP TABLE` and remain approved, by two
-- administrators, with their names and timestamps intact, and the gate would
-- open. The endpoints would still be the ones that were reviewed. The route
-- between them would not.
--
-- So a decision now also records a digest of the statements it was made about,
-- covering the migration and its revert together, because they are reviewed and
-- approved as one. Any edit to either changes the digest and every approval
-- stops matching — the same mechanism regeneration already relied on, extended
-- to the case where the endpoints stay put and the path between them moves.

ALTER TABLE schemaver.migration
    ADD COLUMN plan_digest text NOT NULL DEFAULT '';

ALTER TABLE schemaver.review_decision
    -- Nullable: decisions recorded before this column existed have their digest
    -- backfilled below, but a null here must never be read as "matches".
    ADD COLUMN plan_digest text;

-- Backfill. Existing decisions were made against the statements that are still
-- stored, so filling both sides with the same value preserves approvals that
-- are genuinely still valid rather than invalidating work already done.
--
-- Digested over the statement text in order, main then revert, with the
-- ordinals included so that reordering two statements changes the digest even
-- when the text does not.
WITH plan AS (
    SELECT m.id,
           encode(sha256(convert_to(
               COALESCE((SELECT string_agg(s.ordinal || ':' || s.sql, E'\n' ORDER BY s.ordinal)
                           FROM schemaver.migration_step s
                          WHERE s.migration_id = m.id), '')
               || E'\n--\n' ||
               COALESCE((SELECT string_agg(r.ordinal || ':' || r.sql, E'\n' ORDER BY r.ordinal)
                           FROM schemaver.migration_revert_step r
                          WHERE r.migration_id = m.id), '')
           , 'UTF8')), 'hex') AS digest
      FROM schemaver.migration m
)
UPDATE schemaver.migration m
   SET plan_digest = plan.digest
  FROM plan
 WHERE plan.id = m.id;

UPDATE schemaver.review_decision d
   SET plan_digest = m.plan_digest
  FROM schemaver.migration m
 WHERE m.id = d.migration_id AND d.plan_digest IS NULL;
