-- A drift record cannot outlive the database it compares against.
--
-- `peer_database_id` was declared ON DELETE SET NULL while
-- `drift_peer_present_iff_peer_source` requires it to be present whenever the
-- expected schema came from a peer. The two contradict each other: deleting a
-- database that appears as anybody's peer fires the SET NULL, which then
-- violates the CHECK, and the delete fails with an error naming a constraint on
-- a table the caller never mentioned.
--
-- Nothing in the product deletes a database — decommissioning retires it, which
-- is the whole point of 0010 — so this has sat latent. It surfaced in test
-- cleanup, which is the only thing that deletes, and it would have surfaced the
-- first time anyone tried to remove a database for real.
--
-- CASCADE rather than relaxing the CHECK. A drift record says "this database
-- does not match that one"; with that one gone the statement is not weaker, it
-- is meaningless, and keeping it as a row with a null where the comparison used
-- to be would leave a record nobody can interpret.

ALTER TABLE schemaver.drift
    DROP CONSTRAINT drift_peer_database_id_fkey;

ALTER TABLE schemaver.drift
    ADD CONSTRAINT drift_peer_database_id_fkey
        FOREIGN KEY (peer_database_id)
        REFERENCES schemaver.database (id) ON DELETE CASCADE;
