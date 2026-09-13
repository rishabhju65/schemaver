-- A line of schema development that is not a database.
--
-- The product was built to branch, diff and merge schemas. The diff was there
-- from the start and D-027 made the merge real, but "branch" had nowhere to
-- live: every schema in the system arrived by being read off a database, so the
-- only way to evolve one independently was to have a spare database lying
-- around and remember what it was for. That is not a branch, it is a
-- convention, and nothing enforced or recorded it.
--
-- A branch is cut from a database at a known schema and evolved by writing DDL.
-- No database backs it. Each write is applied to a throwaway copy built at the
-- branch's current schema, which is the same machinery the rehearsal and
-- authored changes already use (D-009): PostgreSQL itself says whether the
-- statements work and what they produce, so a branch head is a schema that
-- genuinely exists rather than one schemaver believes in.
--
-- The point of recording the cut is the merge. D-027 recovers a common ancestor
-- from the snapshots two databases have left behind, which is a good answer to
-- a question that had no better one — it depends on the two having been read at
-- the same schema at some point, and it is a heuristic. A branch knows exactly
-- where it came from, so merging one back is three-way with a base that is a
-- fact rather than a reconstruction.

CREATE TABLE schemaver.branch (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL
        REFERENCES schemaver.project (id) ON DELETE CASCADE,

    -- Chosen by a person, and the handle they will use for it.
    name text NOT NULL,
    description text,

    -- What it was cut from. The database is kept for display and for the
    -- ordinary case of merging back into it, but it is deliberately not what
    -- makes the branch work: base_fingerprint is, and that stays true however
    -- the origin database moves afterwards.
    --
    -- ON DELETE SET NULL rather than CASCADE: retiring or removing the database
    -- a branch came from does not invalidate the branch, whose schemas are all
    -- stored independently.
    origin_database_id bigint
        REFERENCES schemaver.database (id) ON DELETE SET NULL,

    -- The schema at the cut. This is the merge ancestor, exactly, for as long
    -- as the branch exists.
    base_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    -- Where the branch is now. Equal to base_fingerprint on a branch nobody has
    -- written to yet, which is the correct reading: it has diverged by nothing.
    head_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    created_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- The write waiting to be applied, and why the last one was not.
    --
    -- Held on the branch rather than in a queue of its own, which is what makes
    -- "one write in flight at a time" a property of the shape instead of a rule
    -- somebody has to enforce. Statements are composed against the head, so two
    -- overlapping writes would both build on the schema before either landed
    -- and the second would quietly undo the first.
    --
    -- The error is on the branch for the same reason it is not on a commit: a
    -- write that failed produced no commit. That is what failing means here.
    pending_sql text,
    pending_message text,
    pending_author bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    write_error text,

    closed_at timestamptz,
    closed_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    closed_reason text,

    -- One name per project, and names are reused after closing on purpose: a
    -- branch is a working area, and refusing to let somebody cut `add-channel`
    -- again because a finished one had that name would be an obstruction with
    -- nothing behind it.
    CONSTRAINT branch_name_is_unique_while_open
        EXCLUDE (project_id WITH =, name WITH =) WHERE (closed_at IS NULL)
);

CREATE INDEX branch_by_project ON schemaver.branch (project_id, created_at DESC);

-- What happened on a branch, in order.
--
-- Kept as its own rows rather than derived from the fingerprints, because the
-- statements somebody wrote are not recoverable from the two schemas they moved
-- between: a diff of the endpoints says what changed, never how it was
-- expressed, and a branch is a place where the wording is the work.
CREATE TABLE schemaver.branch_commit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    branch_id bigint NOT NULL
        REFERENCES schemaver.branch (id) ON DELETE CASCADE,
    ordinal integer NOT NULL,

    from_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,
    to_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    -- As written, not as re-rendered. The whole point of writing DDL against a
    -- branch is that the author's wording is what runs.
    sql text NOT NULL,
    -- What the change list came to, so the branch page can show a commit
    -- without rebuilding both schemas to diff them.
    changes jsonb NOT NULL DEFAULT '[]'::jsonb,

    message text,
    author_id bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (branch_id, ordinal),
    -- A commit that goes nowhere is a write that did nothing, and those are
    -- refused rather than recorded.
    CONSTRAINT branch_commit_goes_somewhere CHECK (from_fingerprint <> to_fingerprint)
);
