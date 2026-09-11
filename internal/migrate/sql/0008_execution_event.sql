-- The activity log of one migration run: an append-only narrative of what
-- happened, beside the execution row that says what is happening now.
--
-- The two answer different questions and neither replaces the other. The
-- execution row is overwritten on every sample, so it answers "what is it doing
-- right now" in a single row read. It cannot answer "it waited eleven minutes
-- on a session running a report, then built the index in four" — by the time
-- anyone asks, the samples that would have said so have been overwritten.
--
-- Events record transitions, not samples. The observer wakes every two seconds,
-- so a forty-minute migration produces around twelve hundred observations of
-- which nearly all are identical. Writing each one would bury the four moments
-- that matter in noise and put a steady write load on the metadata database for
-- the privilege. An event is written when what we observe changes: a wait
-- begins, a wait ends, a phase changes, progress crosses a threshold.
--
-- Append-only, and deliberately so: rows are never updated. That makes the log
-- of a settled execution immutable, which is what lets it be rolled into a
-- single object in blob storage later and the rows dropped — the pointer would
-- live here, the bytes elsewhere. Nothing in this table needs to change for
-- that to happen, which is the point.

CREATE TABLE schemaver.execution_event (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    execution_id bigint NOT NULL
        REFERENCES schemaver.execution (id) ON DELETE CASCADE,

    at timestamptz NOT NULL DEFAULT now(),

    -- The statement this concerns, where it concerns one, so the interface can
    -- show events under the statement they belong to rather than in one flat
    -- stream. Null for events about the run as a whole.
    ordinal integer,

    -- Severity, so a page can surface the two things a viewer acts on — being
    -- blocked, and failing — without reading the whole log.
    level text NOT NULL DEFAULT 'info'
        CHECK (level IN ('info', 'warn', 'error')),

    -- A stable machine-readable name for what happened ('wait.began',
    -- 'step.committed'). Free text rather than a CHECK: this vocabulary will
    -- grow with every kind of thing worth reporting, and a constraint here
    -- would turn each addition into a migration for no protection worth having.
    kind text NOT NULL,

    -- What a person reads.
    message text NOT NULL,

    -- The structured form of the same thing: blocking pids, wait events, index
    -- phases. Held as one column because the fields differ per kind and
    -- modelling each would mean a column nothing else ever reads.
    detail jsonb
);

-- Ordered by id, not at: two events in the same run can land in the same
-- millisecond, and identity gives them a stable order where a timestamp does
-- not.
CREATE INDEX execution_event_by_execution
    ON schemaver.execution_event (execution_id, id);
