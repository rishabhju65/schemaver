-- How much ceremony a project wants between a change and a database.
--
-- The gate held eight conditions and every project got all of them. Most are
-- not opinions: a migration that failed its rehearsal does not produce the
-- schema it claims, a reviewer who has objected has objected, and an unanswered
-- rename question is a column's data waiting on one click. Those are
-- correctness and stay where they are.
--
-- Two were policy wearing correctness's clothes. Requiring a written way back
-- (D-012, D-022) and requiring an administrator's approval are both right for a
-- team changing production and both pure friction for one person adding a
-- nullable column to a database only they use. The product was built for the
-- first case and gave the second no way out, which is how a tool for making
-- schema changes safely becomes a tool people work around.
--
-- Defaults reproduce exactly what every project has today, so this migration
-- changes nobody's behaviour until somebody chooses to change it.
CREATE TABLE schemaver.project_policy (
    project_id bigint PRIMARY KEY
        REFERENCES schemaver.project (id) ON DELETE CASCADE,

    -- How many project administrators must approve before a change can run.
    --
    -- Zero is allowed and is the point: a project where the only administrator
    -- is also the only author gains nothing from approving their own work, and
    -- the self-approval record exists to make that visible rather than to make
    -- it required.
    approvals_required integer NOT NULL DEFAULT 1
        CHECK (approvals_required >= 0 AND approvals_required <= 5),

    -- Whether somebody must write the way back, or say why there is not one,
    -- before a change can run.
    --
    -- Turning this off does not make a revert unchecked: a revert somebody
    -- writes anyway is still rehearsed, and one that does not lead back still
    -- blocks. What it gives up is being asked at all.
    revert_required boolean NOT NULL DEFAULT true,

    updated_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE schemaver.project_policy IS
    'Per-project gate settings. A project with no row here gets the defaults, '
    'which are what every project had before this table existed.';
