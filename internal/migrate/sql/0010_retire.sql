-- Retiring a database: schemaver is done with it, and the record stays.
--
-- Deliberately not `archived_at`, which already exists and means something else.
-- That column records an observed fact — discovery noticed the database is no
-- longer on the server — and discovery clears it again if the database
-- reappears. Retirement is an intent, decided by a person. Reusing one column
-- for both would mean the next discovery run silently un-retired a database
-- somebody had deliberately stood down.
--
-- Nor is it `managed`, which is a pause: stop watching this, resume later, no
-- consequences either way. Retiring ends the relationship — it cancels queued
-- work, closes open change requests and drift, and refuses every write
-- afterwards. A settings checkbox should not do that quietly, so it gets its
-- own verb.
--
-- Nothing is deleted. Snapshots, change requests, migrations, executions and
-- activity all remain readable; the database simply stops being somewhere
-- changes can be sent.

ALTER TABLE schemaver.database
    ADD COLUMN retired_at timestamptz,
    -- Who decided. ON DELETE SET NULL rather than CASCADE: removing the person
    -- must not un-retire the database.
    ADD COLUMN retired_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    -- Why, in their words. The one piece of context that cannot be recovered
    -- later from anything else.
    ADD COLUMN retired_reason text;

-- Partial, because the interesting set is the small one: live databases are
-- read constantly and retired ones are read when somebody goes looking.
CREATE INDEX database_retired ON schemaver.database (retired_at)
    WHERE retired_at IS NOT NULL;
