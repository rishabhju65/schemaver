-- Connection capacity, sampled from each instance.
--
-- A fixed concurrency cap is wrong in both directions: absurdly low on a server
-- configured for a thousand connections, and dangerous on a small one already
-- near its limit. What matters is neither our configuration nor theirs, but the
-- headroom actually left.

ALTER TABLE schemaver.instance
    -- What the server permits, and what it holds back for superusers. Read from
    -- the server itself; they change only when someone reconfigures it.
    ADD COLUMN max_connections      integer,
    ADD COLUMN reserved_connections integer,

    -- How many connections were in use when last sampled. This is the number
    -- that decides our budget: a server allowing 500 with 480 in use has twenty
    -- spare, and taking a hundred would break their application rather than
    -- ours.
    ADD COLUMN used_connections integer,
    ADD COLUMN capacity_sampled_at timestamptz;

-- The heaviest change class in a migration, which decides how much of an
-- instance's budget executing it consumes.
--
-- Connections are not the binding constraint for a migration: two concurrent
-- index builds compete for the same buffer cache and write-ahead log however
-- many slots are free. So weight is derived from cost rather than from
-- connection count, using the classification the diff engine already produces.
ALTER TABLE schemaver.migration
    ADD COLUMN weight integer NOT NULL DEFAULT 8
        CHECK (weight > 0);

COMMENT ON COLUMN schemaver.migration.weight IS
    'Scheduling cost: 1 metadata-only or additive, 4 lock-heavy, 8 rewriting or destructive. Unknown defaults to the most expensive.';

-- Jobs carry the weight of the work they represent so the claim query can
-- enforce a budget without joining through to the migration.
ALTER TABLE schemaver.job
    ADD COLUMN weight integer NOT NULL DEFAULT 1 CHECK (weight > 0);

CREATE INDEX job_running_weight_idx
    ON schemaver.job (instance_id, weight)
    WHERE state = 'running';
