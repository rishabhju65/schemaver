-- Accounts: isolation between people who share a deployment.
--
-- An account owns instances, credentials, repositories and environments. Users
-- belong to an account. Nothing an account owns is reachable from another,
-- enforced in the application by a store that cannot express an unscoped query.
--
-- The tenant is an account rather than a user so that inviting a second person
-- to an existing set of databases later needs no migration — a user gains an
-- account_id, and the resources do not move.

CREATE TABLE schemaver.account (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Everything an account owns. Nullable first so existing rows can be adopted
-- below, then made mandatory — a row owned by nobody would be invisible to every
-- scoped query and effectively lost.
ALTER TABLE schemaver.app_user
    ADD COLUMN account_id bigint REFERENCES schemaver.account (id) ON DELETE CASCADE;
ALTER TABLE schemaver.instance
    ADD COLUMN account_id bigint REFERENCES schemaver.account (id) ON DELETE CASCADE;
ALTER TABLE schemaver.credential
    ADD COLUMN account_id bigint REFERENCES schemaver.account (id) ON DELETE CASCADE;
ALTER TABLE schemaver.repository
    ADD COLUMN account_id bigint REFERENCES schemaver.account (id) ON DELETE CASCADE;
ALTER TABLE schemaver.environment
    ADD COLUMN account_id bigint REFERENCES schemaver.account (id) ON DELETE CASCADE;

-- Adopt whatever already exists into one account, so an upgrade keeps working
-- rather than silently hiding every registered database.
DO $$
DECLARE
    existing bigint;
BEGIN
    IF EXISTS (SELECT 1 FROM schemaver.instance)
       OR EXISTS (SELECT 1 FROM schemaver.app_user)
       OR EXISTS (SELECT 1 FROM schemaver.environment) THEN
        INSERT INTO schemaver.account (name) VALUES ('Default') RETURNING id INTO existing;
        UPDATE schemaver.app_user    SET account_id = existing WHERE account_id IS NULL;
        UPDATE schemaver.instance    SET account_id = existing WHERE account_id IS NULL;
        UPDATE schemaver.credential  SET account_id = existing WHERE account_id IS NULL;
        UPDATE schemaver.repository  SET account_id = existing WHERE account_id IS NULL;
        UPDATE schemaver.environment SET account_id = existing WHERE account_id IS NULL;
    END IF;
END $$;

ALTER TABLE schemaver.app_user    ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE schemaver.instance    ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE schemaver.credential  ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE schemaver.repository  ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE schemaver.environment ALTER COLUMN account_id SET NOT NULL;

-- Uniqueness becomes per-account. Two accounts naming a server "production" is
-- ordinary; a global constraint would leak the existence of another account's
-- resources by refusing the name.
ALTER TABLE schemaver.instance   DROP CONSTRAINT instance_name_key;
ALTER TABLE schemaver.credential DROP CONSTRAINT credential_name_key;
ALTER TABLE schemaver.repository DROP CONSTRAINT repository_name_key;
ALTER TABLE schemaver.environment DROP CONSTRAINT environment_name_key;
ALTER TABLE schemaver.environment DROP CONSTRAINT environment_rank_key;

CREATE UNIQUE INDEX instance_name_per_account    ON schemaver.instance (account_id, name);
CREATE UNIQUE INDEX credential_name_per_account  ON schemaver.credential (account_id, name);
CREATE UNIQUE INDEX repository_name_per_account  ON schemaver.repository (account_id, name);
CREATE UNIQUE INDEX environment_name_per_account ON schemaver.environment (account_id, name);
CREATE UNIQUE INDEX environment_rank_per_account ON schemaver.environment (account_id, rank);

-- The endpoint index was global too: two accounts may legitimately register the
-- same host, and refusing the second would disclose the first.
DROP INDEX IF EXISTS schemaver.instance_endpoint_key;
CREATE UNIQUE INDEX instance_endpoint_per_account
    ON schemaver.instance (account_id, host, port) WHERE archived_at IS NULL;

CREATE INDEX instance_account_idx    ON schemaver.instance (account_id);
CREATE INDEX app_user_account_idx    ON schemaver.app_user (account_id);
CREATE INDEX environment_account_idx ON schemaver.environment (account_id);

-- Seeded environments belonged to no account and would be invisible to every
-- scoped query. New accounts get their own set at sign-up.
DELETE FROM schemaver.environment
 WHERE account_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM schemaver.database d WHERE d.environment_id = environment.id);
