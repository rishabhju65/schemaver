-- The fingerprint chain records states no database was ever observed at.
--
-- `expected_after` was declared in 0001 as a reference into `schema_blob`, on
-- the assumption that any fingerprint worth recording would be one whose schema
-- we had stored. That holds for everything else: a database's current
-- fingerprint, a migration's start and target, a snapshot — each of those is a
-- schema somebody read off a real database and kept.
--
-- The intermediate states of a migration are not like that. "What the schema
-- looks like after statement four" exists for a few milliseconds inside a
-- shadow database and is never anywhere else. The first proof to run failed on
-- exactly this: every statement applied, the chain was computed correctly, and
-- writing it down violated the constraint.
--
-- Storing the schemas to satisfy the reference was the other option and it is
-- the wrong one. A fourteen-statement migration would keep fourteen complete
-- copies of the schema, differing by one change each, for the sake of states
-- nobody will ever open — hundreds of kilobytes per migration to make a
-- constraint true.
--
-- The fingerprint alone does the whole job anyway. It is compared, never
-- dereferenced: the executor asks "is the database sitting at the value
-- recorded for statement four", and a bare identity answers that completely.

ALTER TABLE schemaver.migration_step
    DROP CONSTRAINT migration_step_expected_after_fkey;
