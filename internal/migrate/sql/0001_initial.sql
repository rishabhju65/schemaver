-- schemaver metadata schema, initial.
--
-- Two Postgres schemas, deliberately:
--
--   schemaver        derived and operational state. Rebuildable by re-reading
--                    the repo and re-introspecting every database. Losing it
--                    costs time, not history.
--   schemaver_audit  the execution and approval record. Authoritative and
--                    irreproducible per D-004 — nothing else can recreate who
--                    did what. It is append-only and must be backed up.
--
-- Keeping them in separate schemas makes D-004's scope cut enforceable rather
-- than aspirational, and reduces backup guidance to one sentence: always dump
-- schemaver_audit; schemaver can be rebuilt.

CREATE SCHEMA IF NOT EXISTS schemaver;
CREATE SCHEMA IF NOT EXISTS schemaver_audit;

-- ---------------------------------------------------------------- environment

-- Environment is an entity rather than an enum because rank is semantic: it
-- decides what "promote to the next tier" means, and Phase 2 attaches approval
-- policy and safety-rule configuration per environment. An enum can carry
-- neither.
CREATE TABLE schemaver.environment (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL UNIQUE,
    -- Unique, so the ordering is total. Ties would make "must reach staging
    -- before production" ambiguous, which is exactly the guarantee rank exists
    -- to provide. Gaps are intentional, to leave room for insertion.
    rank       integer     NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schemaver.environment (name, rank) VALUES
    ('development', 10),
    ('staging',     20),
    ('production',  30);

-- ----------------------------------------------------------------- credential

-- Credentials live apart from the instances that use them so that a credential
-- can become a reference into an external secret manager without touching the
-- instance table, and so one credential can serve several instances.
CREATE TABLE schemaver.credential (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name     text NOT NULL UNIQUE,
    username text NOT NULL,
    kind     text NOT NULL CHECK (kind IN ('inline', 'secret_ref')),

    -- Inline secrets are stored encrypted with a key supplied by the
    -- environment. key_id records which key, so a key rotation can find the
    -- rows it still has to re-encrypt.
    secret_ciphertext bytea,
    key_id            text,

    -- A secret_ref names the secret in an external manager; nothing sensitive
    -- is stored here at all.
    secret_ref text,

    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,

    -- The two kinds are mutually exclusive, enforced here rather than trusted to
    -- application code: a credential row with neither payload is unusable, and
    -- one with both is ambiguous about which is authoritative.
    CONSTRAINT credential_payload_matches_kind CHECK (
        (kind = 'inline'
            AND secret_ciphertext IS NOT NULL AND key_id IS NOT NULL
            AND secret_ref IS NULL)
     OR (kind = 'secret_ref'
            AND secret_ref IS NOT NULL
            AND secret_ciphertext IS NULL AND key_id IS NULL)
    )
);

-- ------------------------------------------------------------------- instance

CREATE TABLE schemaver.instance (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text    NOT NULL UNIQUE,
    host text    NOT NULL,
    port integer NOT NULL DEFAULT 5432 CHECK (port BETWEEN 1 AND 65535),

    -- PostgreSQL only, per D-003, asserted rather than abstracted: a single
    -- constrained column, no dialect registry and no plugin interface. Adding an
    -- engine later relaxes this CHECK instead of adding a column to a populated
    -- table.
    engine text NOT NULL DEFAULT 'postgres' CHECK (engine IN ('postgres')),

    tls_mode text NOT NULL DEFAULT 'require'
        CHECK (tls_mode IN ('disable', 'allow', 'prefer', 'require',
                            'verify-ca', 'verify-full')),
    -- Managed Postgres generally requires the provider's CA bundle.
    tls_root_cert text,

    -- RESTRICT, not CASCADE: deleting a credential must not silently orphan the
    -- instances that authenticate with it.
    credential_id bigint NOT NULL
        REFERENCES schemaver.credential (id) ON DELETE RESTRICT,

    engine_version text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Archive rather than delete, so snapshot history stays coherent after an
    -- instance is decommissioned.
    archived_at    timestamptz
);

-- Registering the same server twice produces two divergent histories of one
-- reality. Archived rows are excluded so a decommissioned host can be
-- re-registered later.
CREATE UNIQUE INDEX instance_endpoint_key
    ON schemaver.instance (host, port) WHERE archived_at IS NULL;

-- ------------------------------------------------------------------- database

CREATE TABLE schemaver.database (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    instance_id bigint NOT NULL
        REFERENCES schemaver.instance (id) ON DELETE RESTRICT,
    name        text NOT NULL,

    environment_id bigint
        REFERENCES schemaver.environment (id) ON DELETE SET NULL,

    -- Databases are discovered by enumerating the instance; managed marks the
    -- ones the operator actually ticked. Discovery must not imply consent.
    managed boolean NOT NULL DEFAULT false,

    -- Last values reported by the instance, for the fleet dashboard.
    owner      text,
    encoding   text,
    size_bytes bigint,

    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,

    UNIQUE (instance_id, name)
);

CREATE INDEX database_managed_idx
    ON schemaver.database (instance_id) WHERE managed AND archived_at IS NULL;

-- ---------------------------------------------------------------- schema_blob

-- Schemas are content-addressed, so identical schemas are stored once no matter
-- how many databases hold them or how often they are observed. A database
-- introspected hourly for a year without changing stores one blob and 8,760
-- snapshot rows.
CREATE TABLE schemaver.schema_blob (
    fingerprint text PRIMARY KEY CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    canonical   jsonb       NOT NULL,
    table_count integer     NOT NULL,
    bytes       integer     NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------- snapshot

-- A snapshot row is written only when the schema *changed*, or when a read
-- failed. Observations that found no change write nothing at all, so a database
-- that never changes costs no storage however often it is polled — the retention
-- problem is designed away rather than managed. Liveness lives on the database
-- row instead (last_checked_at), and this table is exactly the change history
-- the timeline UI wants, with no filtering.
CREATE TABLE schemaver.snapshot (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL
        REFERENCES schemaver.database (id) ON DELETE CASCADE,

    -- Null when the read failed. A failed introspection is still an
    -- observation — recording it here rather than elsewhere keeps "when did we
    -- last successfully read this" a single query.
    fingerprint text
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    observed_at timestamptz NOT NULL DEFAULT now(),
    read_ms     integer,
    error       text,

    CONSTRAINT snapshot_is_result_or_error CHECK (
        (fingerprint IS NOT NULL AND error IS NULL)
     OR (fingerprint IS NULL     AND error IS NOT NULL)
    )
);

CREATE INDEX snapshot_database_time_idx
    ON schemaver.snapshot (database_id, observed_at DESC);

-- Current observed state, denormalized onto the database row.
--
-- All of it is derived: re-introspecting rebuilds every column here, so it sits
-- squarely in D-004's disposable half. It exists so the fleet dashboard reads one
-- table instead of computing a per-database maximum over snapshot history.
ALTER TABLE schemaver.database
    ADD COLUMN current_fingerprint text
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,
    -- No foreign key: this and snapshot.database_id would form a cycle, and the
    -- column is a convenience pointer that must never block deleting a snapshot.
    ADD COLUMN current_snapshot_id bigint,

    -- The cheap change-detection digest from the last probe. Not a fingerprint:
    -- it is computed over raw catalog fields and is only ever compared with
    -- itself, never used as identity.
    ADD COLUMN probe_digest text,

    -- last_checked_at moves on every successful probe; last_read_at only on a
    -- full introspection. The gap between them is how stale the schema view is,
    -- which the UI must show rather than presenting old data as current.
    ADD COLUMN last_checked_at timestamptz,
    ADD COLUMN last_read_at    timestamptz,
    ADD COLUMN last_error      text;

-- ----------------------------------------------------------------- repository

CREATE TABLE schemaver.repository (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    url         text NOT NULL,
    branch      text NOT NULL DEFAULT 'main',
    schema_path text NOT NULL DEFAULT 'schema',
    credential_id bigint
        REFERENCES schemaver.credential (id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- What a database is expected to match. Exactly one of two sources, or neither:
--
--   repository_id     the declared schema in a repository. Several databases
--                     point at one repository — staging and production normally
--                     share declared intent, and that shared expectation is what
--                     makes drift meaningful.
--   expected_peer_id  another live database. This is what makes the product
--                     useful before any repository is connected: two connection
--                     strings and "staging and production differ in these three
--                     ways" is already an answer most teams do not have.
--
-- Neither set is a legitimate state, not a half-configured one: the database is
-- observed and explorable, it simply has nothing to be compared against.
ALTER TABLE schemaver.database
    ADD COLUMN repository_id bigint
        REFERENCES schemaver.repository (id) ON DELETE SET NULL,
    ADD COLUMN expected_peer_id bigint
        REFERENCES schemaver.database (id) ON DELETE SET NULL,

    -- Two expectations would be two answers to one question.
    ADD CONSTRAINT database_single_expectation CHECK (
        repository_id IS NULL OR expected_peer_id IS NULL),
    -- A database cannot drift from itself.
    ADD CONSTRAINT database_peer_is_not_self CHECK (
        expected_peer_id IS NULL OR expected_peer_id <> id);

CREATE TABLE schemaver.declared_schema (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repository_id bigint NOT NULL
        REFERENCES schemaver.repository (id) ON DELETE CASCADE,
    git_ref     text NOT NULL,
    fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,
    imported_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (repository_id, git_ref)
);

-- ---------------------------------------------------------------------- drift

-- Drift is state, not an event stream. A divergence that persists for a week is
-- one row with a moving last_seen, not 168 rows — otherwise the interface
-- teaches people to ignore it, which is the precise failure drift detection
-- exists to prevent.
CREATE TABLE schemaver.drift (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL
        REFERENCES schemaver.database (id) ON DELETE CASCADE,

    observed_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,
    expected_fingerprint text NOT NULL
        REFERENCES schemaver.schema_blob (fingerprint) ON DELETE RESTRICT,

    -- 'declared' compares against the repository. 'peer' compares against
    -- another live database, which is what makes the tool useful before any
    -- repository has been connected.
    expected_source  text NOT NULL CHECK (expected_source IN ('declared', 'peer')),
    peer_database_id bigint REFERENCES schemaver.database (id) ON DELETE SET NULL,

    status text NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'resolved', 'ignored')),

    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,

    CONSTRAINT drift_peer_present_iff_peer_source CHECK (
        (expected_source = 'peer'     AND peer_database_id IS NOT NULL)
     OR (expected_source = 'declared' AND peer_database_id IS NULL)
    ),
    CONSTRAINT drift_is_a_difference CHECK (
        observed_fingerprint <> expected_fingerprint
    )
);

-- One open row per distinct divergence; re-observing it moves last_seen.
CREATE UNIQUE INDEX drift_open_key
    ON schemaver.drift (database_id, observed_fingerprint, expected_fingerprint)
    WHERE status = 'open';

-- ------------------------------------------------------------------------ job

CREATE TABLE schemaver.job (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL,

    target_kind text,
    target_id   bigint,

    -- Denormalized so the claim query can cap concurrency per instance without a
    -- join. Enforcing the limit in the queue rather than in worker memory means
    -- it holds across every worker process, not just within one.
    instance_id bigint REFERENCES schemaver.instance (id) ON DELETE CASCADE,

    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),

    attempts  integer     NOT NULL DEFAULT 0,
    run_after timestamptz NOT NULL DEFAULT now(),

    -- A worker holds a lease rather than a lock, so a job whose worker died is
    -- reclaimed when the lease lapses instead of sitting in 'running' forever.
    lease_until timestamptz,
    worker_id   text,

    -- Set by the caller so a retried request cannot enqueue the same work twice.
    idempotency_key text UNIQUE,

    started_at  timestamptz,
    finished_at timestamptz,
    error       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX job_claimable_idx
    ON schemaver.job (run_after)
    WHERE state = 'pending';

-- Supports counting a single instance's in-flight jobs during claim.
CREATE INDEX job_running_per_instance_idx
    ON schemaver.job (instance_id)
    WHERE state = 'running';

-- ----------------------------------------------------------------- identity

CREATE TABLE schemaver.app_user (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text NOT NULL UNIQUE,
    display_name  text,
    password_hash text NOT NULL,
    role          text NOT NULL DEFAULT 'viewer'
        CHECK (role IN ('admin', 'operator', 'viewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz
);

CREATE TABLE schemaver.session (
    -- The token itself is never stored; a leaked database must not yield live
    -- sessions.
    token_hash   bytea PRIMARY KEY,
    user_id      bigint NOT NULL
        REFERENCES schemaver.app_user (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz
);

CREATE INDEX session_expiry_idx ON schemaver.session (expires_at);

-- ---------------------------------------------------------------------- audit

CREATE TABLE schemaver_audit.event (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),

    -- Deliberately not a foreign key. The audit trail has to outlive the rows it
    -- refers to: deleting a user must never delete or invalidate the record of
    -- what they did. actor_label carries the identity in readable form so the
    -- entry still means something once the user is gone.
    actor_user_id bigint,
    actor_label   text NOT NULL,

    action        text NOT NULL,
    subject_kind  text,
    subject_id    bigint,
    subject_label text,

    detail jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_event_time_idx    ON schemaver_audit.event (at DESC);
CREATE INDEX audit_event_actor_idx   ON schemaver_audit.event (actor_user_id, at DESC);
CREATE INDEX audit_event_subject_idx ON schemaver_audit.event (subject_kind, subject_id, at DESC);

-- Append-only, enforced by the database. Application discipline is not enough
-- for the one table that cannot be reconstructed if it is wrong.
CREATE FUNCTION schemaver_audit.deny_mutation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'schemaver_audit.event is append-only';
END;
$$;

CREATE TRIGGER event_append_only
    BEFORE UPDATE OR DELETE ON schemaver_audit.event
    FOR EACH ROW EXECUTE FUNCTION schemaver_audit.deny_mutation();
