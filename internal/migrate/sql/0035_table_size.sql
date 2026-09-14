-- How much of each table there is.
--
-- The product classified a change by what it does and never by what it does it
-- to. `lock_heavy` on a table with four hundred million rows and `lock_heavy`
-- on one somebody created this morning were the same symbol, so nothing could
-- warn about the first, order a plan by what would hurt, or choose a timeout
-- that suited the table. A rehearsal against a throwaway copy establishes that
-- a migration is correct and says nothing about what it will cost, and its own
-- comment admits as much.
--
-- Kept beside the schema rather than in it, and that is not a filing decision.
-- A schema's version is the digest of its canonical form; a table growing is
-- not a schema change. Putting a size in the model would make every observation
-- of an unchanged schema a new version, and make two databases differ because
-- one has more rows than the other.
CREATE TABLE schemaver.table_size (
    database_id bigint NOT NULL
        REFERENCES schemaver.database (id) ON DELETE CASCADE,
    namespace  text NOT NULL,
    table_name text NOT NULL,

    -- Total on disk: the table, its indexes and its TOAST. The number that
    -- predicts what a rewrite costs, rather than the heap alone.
    bytes bigint NOT NULL,

    -- The planner's row estimate, and whether there is one.
    --
    -- PostgreSQL writes -1 into reltuples for a table it has never analysed,
    -- and the difference between that and zero is why this column has a
    -- companion. A table nobody has analysed is usually a table nobody has
    -- looked at, which is where the unpleasant surprises live; recording it as
    -- empty would turn "we do not know" into the most reassuring answer
    -- available.
    rows_estimate bigint,
    analysed boolean NOT NULL DEFAULT false,

    observed_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (database_id, namespace, table_name)
);

COMMENT ON TABLE schemaver.table_size IS
    'What each table costs to touch, refreshed on every observation. Never part '
    'of a schema fingerprint: a table growing is not a schema change.';
