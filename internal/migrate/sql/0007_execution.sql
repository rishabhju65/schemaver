-- Execution records: what a migration is doing, while it does it.
--
-- Without these a request goes from queued to finished with silence in between,
-- and a viewer cannot tell a forty-minute index build from a hung one. That
-- distinction is the whole reason D-013 calls for a second connection observing
-- from outside: the database reports nothing to the session running the DDL, so
-- progress has to be watched rather than reported.

CREATE TABLE schemaver.execution (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    migration_id bigint NOT NULL
        REFERENCES schemaver.migration (id) ON DELETE CASCADE,

    state text NOT NULL DEFAULT 'running'
        CHECK (state IN ('running', 'completed', 'failed', 'needs_attention', 'stale')),

    statements_total integer     NOT NULL,
    statements_done  integer     NOT NULL DEFAULT 0,
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,

    -- What is happening right now, replaced on every observation. Live state,
    -- not history: the per-statement record below keeps that.
    current_step       integer,
    current_started_at timestamptz,

    -- Who is standing in the way. The most useful thing a person can be handed
    -- while a migration is stuck, and almost nothing offers it: an ALTER waiting
    -- for a lock is waiting on a specific other session, and the remedy is
    -- usually to go and ask that session's owner.
    wait_event   text,
    blocked_by   integer[],
    blocker_query text,

    -- Real progress, where the engine exposes it. Index builds do; table
    -- rewrites do not, and claiming otherwise would be worse than silence.
    progress_phase   text,
    progress_percent numeric(5,2),

    observed_at timestamptz,
    reason      text,
    final_fingerprint text
);

CREATE INDEX execution_migration_idx
    ON schemaver.execution (migration_id, started_at DESC);
CREATE INDEX execution_running_idx
    ON schemaver.execution (state) WHERE state = 'running';

-- One row per statement actually run, so a slow migration can be explained
-- afterwards rather than only watched at the time.
CREATE TABLE schemaver.execution_step (
    execution_id bigint NOT NULL
        REFERENCES schemaver.execution (id) ON DELETE CASCADE,
    ordinal      integer NOT NULL,

    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    error       text,

    PRIMARY KEY (execution_id, ordinal)
);
