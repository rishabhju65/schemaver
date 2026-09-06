-- Organisations and projects.
--
-- The account of 0002 becomes a project: the thing that owns databases. People
-- are granted access to projects rather than owning them, which is what makes
-- ownership survive a reorganisation — a service outlives the team that built
-- it, and one team commonly owns several unrelated services.
--
-- Above projects sits an organisation. Its purpose is the view no project-scoped
-- role can have: seeing every schema in the company at once. An organisation role
-- reads everything and owns nothing, so that visibility never becomes a path to
-- production.

CREATE TABLE schemaver.organization (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Each existing account becomes an organisation containing one project of the
-- same name. Nothing is lost and nobody's access changes.
INSERT INTO schemaver.organization (name, created_at)
SELECT name, created_at FROM schemaver.account ORDER BY id;

ALTER TABLE schemaver.account RENAME TO project;
ALTER TABLE schemaver.project ADD COLUMN organization_id bigint
    REFERENCES schemaver.organization (id) ON DELETE CASCADE;

-- Pair each project with the organisation created from it. Matching on name and
-- creation time is safe here only because the rows were just inserted from these
-- same rows, in this same transaction.
UPDATE schemaver.project p
   SET organization_id = o.id
  FROM schemaver.organization o
 WHERE o.name = p.name AND o.created_at = p.created_at
   AND p.organization_id IS NULL;

ALTER TABLE schemaver.project ALTER COLUMN organization_id SET NOT NULL;

-- Ownership columns follow the rename. The values do not move: a project has the
-- same id its account had.
ALTER TABLE schemaver.instance    RENAME COLUMN account_id TO project_id;
ALTER TABLE schemaver.credential  RENAME COLUMN account_id TO project_id;
ALTER TABLE schemaver.repository  RENAME COLUMN account_id TO project_id;
ALTER TABLE schemaver.environment RENAME COLUMN account_id TO project_id;

ALTER INDEX schemaver.instance_name_per_account     RENAME TO instance_name_per_project;
ALTER INDEX schemaver.credential_name_per_account   RENAME TO credential_name_per_project;
ALTER INDEX schemaver.repository_name_per_account   RENAME TO repository_name_per_project;
ALTER INDEX schemaver.environment_name_per_account  RENAME TO environment_name_per_project;
ALTER INDEX schemaver.environment_rank_per_account  RENAME TO environment_rank_per_project;
ALTER INDEX schemaver.instance_endpoint_per_account RENAME TO instance_endpoint_per_project;
ALTER INDEX schemaver.instance_account_idx          RENAME TO instance_project_idx;
ALTER INDEX schemaver.environment_account_idx       RENAME TO environment_project_idx;

-- ------------------------------------------------------------- membership

-- A user belongs to one organisation and may work in any number of its projects.
-- That is the case flat tenancy could not express: an engineer working across
-- payments and fraud had to pick one.
CREATE TABLE schemaver.project_member (
    project_id bigint NOT NULL REFERENCES schemaver.project (id) ON DELETE CASCADE,
    user_id    bigint NOT NULL REFERENCES schemaver.app_user (id) ON DELETE CASCADE,
    role       text   NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);

CREATE INDEX project_member_user_idx ON schemaver.project_member (user_id);

ALTER TABLE schemaver.app_user
    ADD COLUMN organization_id bigint REFERENCES schemaver.organization (id) ON DELETE CASCADE,
    -- Organisation-level standing, distinct from any project role. 'member' is
    -- ordinary; 'viewer' reads every project and owns nothing, which is the
    -- auditor and platform-team case.
    ADD COLUMN org_role text NOT NULL DEFAULT 'member'
        CHECK (org_role IN ('member', 'viewer'));

-- Existing users join the organisation their account became, and administer the
-- project that came with it.
UPDATE schemaver.app_user u
   SET organization_id = p.organization_id
  FROM schemaver.project p
 WHERE p.id = u.account_id;

INSERT INTO schemaver.project_member (project_id, user_id, role)
SELECT u.account_id, u.id, u.role FROM schemaver.app_user u
    ON CONFLICT DO NOTHING;

ALTER TABLE schemaver.app_user ALTER COLUMN organization_id SET NOT NULL;
-- Dropping the column also drops the index over it, so the replacement is
-- created rather than renamed.
ALTER TABLE schemaver.app_user DROP COLUMN account_id;
CREATE INDEX app_user_organization_idx ON schemaver.app_user (organization_id);

CREATE INDEX project_organization_idx ON schemaver.project (organization_id);
