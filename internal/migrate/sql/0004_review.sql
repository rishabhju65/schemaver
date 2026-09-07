-- Change requests, generated migrations, and review.
--
-- Two properties in here do most of the work, and both come from things decided
-- earlier:
--
--   A review decision records the fingerprint pair it was made about. So when a
--   migration is regenerated, prior approvals simply stop matching — nothing has
--   to invalidate them, no job sweeps them, and no state goes stale. D-017's
--   "evidence expires" becomes a property of the data rather than a process.
--
--   A comment thread is anchored to a *semantic change identity*
--   ("drop_column:public.orders.legacy_status"), never to a line or an offset.
--   Those identities survive regeneration, which is why D-008's review model is
--   possible at all.

CREATE TABLE schemaver.change_request (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES schemaver.project (id) ON DELETE CASCADE,

    title       text NOT NULL,
    description text,
    author_id   bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,

    -- The database this changes, and where the desired schema comes from. Per
    -- D-018 the target may be another live database rather than authored intent,
    -- in which case source_database_id names it.
    database_id        bigint NOT NULL REFERENCES schemaver.database (id) ON DELETE CASCADE,
    source_database_id bigint REFERENCES schemaver.database (id) ON DELETE SET NULL,

    -- The twelve states of D-017. Everything finer lives in state_reason, which
    -- is where the fifteen conditions that did not earn a state of their own
    -- went.
    state text NOT NULL DEFAULT 'INITIATED' CHECK (state IN (
        'INITIATED', 'IN_REVIEW', 'REVIEWED', 'STAGE_SANITY',
        'READY_TO_EXECUTE', 'EXECUTING', 'COMPLETED',
        'CHANGES_REQUESTED', 'STALE', 'FAILED', 'NEEDS_ATTENTION', 'CLOSED')),
    state_reason text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX change_request_project_idx ON schemaver.change_request (project_id, state);
CREATE INDEX change_request_database_idx ON schemaver.change_request (database_id);

-- --------------------------------------------------------------- migrations

-- A generated migration. Regenerating produces a *new row* rather than updating
-- this one, so what a reviewer approved remains inspectable after the base moves
-- and the migration is rebuilt against it.
CREATE TABLE schemaver.migration (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_request_id bigint NOT NULL
        REFERENCES schemaver.change_request (id) ON DELETE CASCADE,

    -- Identity, per D-007. Applying this to a database not at from_fingerprint
    -- is refused.
    from_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,
    to_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    -- The semantic changes exactly as the diff engine produced them, so the
    -- review surface renders what was generated rather than recomputing it and
    -- risking a different answer.
    changes jsonb NOT NULL,

    -- Rename proposals awaiting a human answer. A migration with unanswered
    -- proposals must not be executed: a rename and a drop-plus-add are
    -- indistinguishable from schema alone, and guessing destroys data.
    rename_candidates jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- Set when no revert can be generated, surfaced at approval time per D-012.
    irreversible_reason text,

    generated_at timestamptz NOT NULL DEFAULT now(),
    superseded_at timestamptz,

    CONSTRAINT migration_goes_somewhere CHECK (from_fingerprint <> to_fingerprint)
);

CREATE INDEX migration_request_idx
    ON schemaver.migration (change_request_id, generated_at DESC);

-- One executable step. expected_after is the fingerprint chain that makes
-- recovery deterministic (D-013): matching a live schema against these
-- checkpoints identifies exactly where a failed migration stopped, with no
-- reliance on our own bookkeeping.
CREATE TABLE schemaver.migration_step (
    migration_id bigint NOT NULL
        REFERENCES schemaver.migration (id) ON DELETE CASCADE,
    ordinal integer NOT NULL,

    sql       text    NOT NULL,
    change_id text    NOT NULL,
    -- False for statements that refuse to run in a transaction, such as a
    -- concurrent index build. A migration containing one is not atomic, and
    -- review has to show that rather than let a failure reveal it (D-006).
    transactional boolean NOT NULL DEFAULT true,
    note      text,

    expected_after text REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    PRIMARY KEY (migration_id, ordinal)
);

-- ------------------------------------------------------------------- review

-- A thread of discussion about one change, or about the request as a whole when
-- anchor is null.
CREATE TABLE schemaver.review_thread (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_request_id bigint NOT NULL
        REFERENCES schemaver.change_request (id) ON DELETE CASCADE,

    -- A semantic change identity, e.g. 'drop_column:public.orders.legacy_status'.
    -- Deliberately not a foreign key: the change it refers to may not exist in
    -- the current migration, and that is meaningful rather than broken — it is
    -- how a thread resolves because the objection was acted on.
    anchor text,

    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),

    -- Why it closed. 'change_removed' is the interesting one: the change the
    -- thread objected to is no longer in the migration, so the thread is
    -- answered by the diff rather than by anybody clicking anything.
    resolution text CHECK (resolution IN ('addressed', 'change_removed', 'declined')),
    resolved_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    resolved_at timestamptz,

    created_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT thread_resolution_matches_status CHECK (
        (status = 'open'     AND resolution IS NULL AND resolved_at IS NULL)
     OR (status = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX review_thread_request_idx
    ON schemaver.review_thread (change_request_id, status);
CREATE INDEX review_thread_anchor_idx
    ON schemaver.review_thread (change_request_id, anchor);

-- Comments within a thread, chronological.
--
-- Two levels rather than arbitrary nesting: a thread is anchored to something,
-- and replies within it are a conversation. Unbounded nesting renders badly and
-- invites five-deep arguments nobody can follow.
CREATE TABLE schemaver.review_comment (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id bigint NOT NULL
        REFERENCES schemaver.review_thread (id) ON DELETE CASCADE,

    author_id bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    -- Denormalised so a comment still reads sensibly after its author's account
    -- is removed. The review record has to outlive the people in it.
    author_label text NOT NULL,

    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    edited_at  timestamptz
);

CREATE INDEX review_comment_thread_idx
    ON schemaver.review_comment (thread_id, created_at);

-- A reviewer's decision, recorded against the exact migration it concerns.
--
-- The fingerprint pair is what makes expiry automatic. Counting approvals means
-- counting decisions whose pair matches the *current* migration, so regenerating
-- one silently withdraws every approval of its predecessor. Nothing invalidates,
-- nothing sweeps, and there is no window in which a stale approval still counts.
CREATE TABLE schemaver.review_decision (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_request_id bigint NOT NULL
        REFERENCES schemaver.change_request (id) ON DELETE CASCADE,
    migration_id bigint NOT NULL
        REFERENCES schemaver.migration (id) ON DELETE CASCADE,

    reviewer_id    bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    reviewer_label text NOT NULL,

    decision text NOT NULL CHECK (decision IN ('approve', 'request_changes', 'reject')),
    comment  text,

    from_fingerprint text NOT NULL,
    to_fingerprint   text NOT NULL,

    decided_at timestamptz NOT NULL DEFAULT now(),

    -- One standing decision per reviewer per migration; a change of mind
    -- replaces it rather than accumulating.
    UNIQUE (migration_id, reviewer_id)
);

CREATE INDEX review_decision_request_idx
    ON schemaver.review_decision (change_request_id, decided_at DESC);
