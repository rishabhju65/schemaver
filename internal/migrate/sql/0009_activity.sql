-- One activity log for the whole product, replacing the execution-only one.
--
-- D-019 recorded what a migration does while it runs. That covers the loudest
-- few minutes of a change request's life and none of the rest: who opened it,
-- what the diff came out as, who approved it, when it was queued, and what the
-- databases were doing in between. A viewer asking "what happened here" was
-- answered for the execution and left to join five tables for everything else.
--
-- Rows are tagged with every entity they concern rather than pointing at one.
-- An execution event carries its execution, its migration, its request and its
-- database, so "everything about request 10" and "everything that happened to
-- shop_prod" are each a single indexed scan with no set of ids to gather first.
-- The duplication is the point: this table is written once and read from many
-- directions, and storage is not the constraint here.
--
-- Deliberately not an audit log. Writes are best-effort and outside the
-- transaction they describe, so a failure to record something never fails the
-- thing itself. That is the right trade for visibility and the wrong one for
-- compliance — an audit record written in a different transaction than the
-- change it describes is a hint, not a record. If compliance-grade audit is
-- ever wanted, it belongs in its own table with the opposite contract rather
-- than as a stricter mode of this one.

CREATE TABLE schemaver.activity (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),

    -- Which project's activity this is. Carried on the row rather than reached
    -- through the entities, so the scope filter is the same one column whatever
    -- the row happens to be about.
    project_id bigint NOT NULL
        REFERENCES schemaver.project (id) ON DELETE CASCADE,

    -- Who caused it. Null means schemaver itself: a worker observing, a job
    -- retrying, a migration applying. The label is denormalized so a removed
    -- user's actions still read as theirs rather than as a dangling id.
    actor_id    bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    actor_label text   NOT NULL DEFAULT 'schemaver',

    -- Severity, so the two things a viewer acts on — something waiting and
    -- something failing — can be found without reading everything.
    level text NOT NULL DEFAULT 'info'
        CHECK (level IN ('info', 'warn', 'error')),

    -- A stable machine-readable name ('request.opened', 'wait.began'). Free
    -- text rather than a CHECK: this vocabulary grows with every new thing
    -- worth reporting, and a constraint would turn each addition into a
    -- migration for no protection worth having.
    kind    text NOT NULL,
    message text NOT NULL,
    detail  jsonb,

    -- Every entity the row concerns, all optional. ON DELETE CASCADE
    -- throughout: activity about something deleted is activity about nothing.
    database_id   bigint REFERENCES schemaver.database (id) ON DELETE CASCADE,
    instance_id   bigint REFERENCES schemaver.instance (id) ON DELETE CASCADE,
    request_id    bigint REFERENCES schemaver.change_request (id) ON DELETE CASCADE,
    migration_id  bigint REFERENCES schemaver.migration (id) ON DELETE CASCADE,
    execution_id  bigint REFERENCES schemaver.execution (id) ON DELETE CASCADE,
    -- The statement within an execution, where there is one.
    ordinal integer
);

-- One index per direction the log is read from. Each is (entity, id) so a
-- timeline comes back already ordered and the sort is free.
CREATE INDEX activity_by_project   ON schemaver.activity (project_id, id DESC);
CREATE INDEX activity_by_request   ON schemaver.activity (request_id, id)
    WHERE request_id IS NOT NULL;
CREATE INDEX activity_by_database  ON schemaver.activity (database_id, id)
    WHERE database_id IS NOT NULL;
CREATE INDEX activity_by_execution ON schemaver.activity (execution_id, id)
    WHERE execution_id IS NOT NULL;

-- Carry the execution log over rather than stranding it. The entity tags are
-- recovered by joining what the old row could only point at indirectly.
INSERT INTO schemaver.activity
       (at, project_id, level, kind, message, detail,
        database_id, instance_id, request_id, migration_id, execution_id, ordinal)
SELECT ev.at, i.project_id, ev.level, ev.kind, ev.message, ev.detail,
       d.id, i.id, r.id, m.id, ev.execution_id, ev.ordinal
  FROM schemaver.execution_event ev
  JOIN schemaver.execution      e ON e.id = ev.execution_id
  JOIN schemaver.migration      m ON m.id = e.migration_id
  JOIN schemaver.change_request r ON r.id = m.change_request_id
  JOIN schemaver.database       d ON d.id = r.database_id
  JOIN schemaver.instance       i ON i.id = d.instance_id
 ORDER BY ev.id;

DROP TABLE schemaver.execution_event;
