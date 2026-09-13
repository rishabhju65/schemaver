-- A change request reaches more than one database.
--
-- Until now a request named one, so getting a change into staging and then
-- production meant two requests: write it for staging, then separately propose
-- bringing production in line. Two reviews, two approvals, two things to watch,
-- for one change — and the second request's statements were generated after the
-- first had been approved, so what ran in production was never quite what
-- anybody reviewed.
--
-- One migration serves every target, and that is a property of the model rather
-- than a convenience. A migration is (from -> to), which describes schemas, not
-- databases; any database sitting at `from` can be taken to `to` by the same
-- statements. Databases in a promotion chain are supposed to be at the same
-- schema — that is what the chain is for — so one migration covers the whole
-- pipeline, and D-024's gate already orders them: production cannot run until
-- staging has reached `to`.
--
-- Each target still gets its own rehearsal against its own starting schema, so
-- a database that has drifted fails its proof rather than executing blind.

CREATE TABLE schemaver.change_request_target (
    change_request_id bigint NOT NULL
        REFERENCES schemaver.change_request (id) ON DELETE CASCADE,
    database_id bigint NOT NULL
        REFERENCES schemaver.database (id) ON DELETE CASCADE,

    -- Where in the chain, counting from the database the change was written
    -- for. Stored rather than re-derived at read time: the Follows mapping can
    -- change after a request is opened, and the order a reviewer approved is
    -- the order that should run.
    position integer NOT NULL,

    PRIMARY KEY (change_request_id, database_id)
);

CREATE INDEX change_request_target_order
    ON schemaver.change_request_target (change_request_id, position);

-- Every request that already exists targets the one database it was opened
-- against, which is what it has always meant.
INSERT INTO schemaver.change_request_target (change_request_id, database_id, position)
SELECT r.id, r.database_id, 0 FROM schemaver.change_request r
ON CONFLICT DO NOTHING;

COMMENT ON COLUMN schemaver.change_request.database_id IS
    'The database this change was written for: the first target, and the one '
    'its migration is generated against. The full set of targets, including '
    'this one, is in change_request_target.';

-- Which database an execution is for.
--
-- A job named a migration and the executor worked out the database from the
-- request, which was unambiguous while a request had one. With a chain the same
-- migration runs against each target in turn, so the job has to say which — and
-- the key that stops a button press queueing twice has to distinguish them too.
ALTER TABLE schemaver.job
    ADD COLUMN database_id bigint REFERENCES schemaver.database (id) ON DELETE CASCADE;
