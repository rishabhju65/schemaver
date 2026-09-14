-- Rollback goes. A change that undoes another is just a change.
--
-- The product required every migration to carry a way back — written by hand or
-- explicitly declared absent — rehearsed it round-trip against a throwaway
-- database, and left a one-press rollback on offer after a successful run. It
-- was better than most: the revert could not go missing from a future commit
-- because migrations live here rather than in a deploy artifact, a half-applied
-- migration refused to be placed rather than guessed at, and the round trip was
-- actually proven. It was still a button almost nobody pressed.
--
-- The field settled this. Flyway put undo behind a paywall and its own
-- documentation says the idea "sometimes breaks down in practice"; Prisma,
-- Supabase and Drizzle ship no down files at all; PlanetScale's revert is capped
-- at thirty minutes, refuses when data integrity is at risk, and was never
-- carried into their PostgreSQL product. Atlas, having interviewed hundreds of
-- engineers, met one team that routinely applied down files in production, and
-- that team was unhappy with it.
--
-- The reason is not discipline, it is information. A revert restores shape and
-- never contents: the inverse of a safe ADD COLUMN is a DROP COLUMN that
-- destroys the data the original change was made to hold. The two moments
-- somebody most wants a rollback — a run that stopped halfway, and data already
-- lost — are the two this product already refused to serve.
--
-- So undoing a change is proposing the reverse, which is reviewed, rehearsed and
-- promoted like anything else. The store's own comment has said so since the
-- rollback was written: "propose the reverse as its own change, reviewed like
-- anything else."

-- The gating columns go, because they gate. Nothing should be able to hold a
-- change on the strength of a feature that no longer exists.
ALTER TABLE schemaver.migration
    DROP COLUMN revert_proof_state,
    DROP COLUMN revert_proof_reason,
    DROP COLUMN revert_authored_at,
    DROP COLUMN revert_author_id,
    DROP COLUMN no_revert_reason,
    DROP COLUMN no_revert_by,
    DROP COLUMN no_revert_at;

-- And the policy switch, which asked whether a project wanted a gate that is no
-- longer there.
ALTER TABLE schemaver.project_policy
    DROP COLUMN revert_required;

-- What stays, and why.
--
-- migration_revert_step keeps the statements people wrote, and execution keeps
-- direction = 'revert' for the runs that happened. A rollback that ran is a
-- thing that happened to a real database, and this product does not delete the
-- record of what happened to a database — retiring, closing and archiving all
-- keep their subject readable for the same reason. Nothing writes to either
-- again.
--
-- REVERTED likewise stays a legal request state. One request in this deployment
-- reached it honestly; removing the label would not remove the event, only the
-- ability to describe it.
COMMENT ON TABLE schemaver.migration_revert_step IS
    'Historical. Reverts authored while migrations were required to carry one. '
    'Nothing writes here since rollback was removed; the rows are kept because '
    'one of them ran against a real database.';
