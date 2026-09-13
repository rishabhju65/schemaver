-- Somebody's answer to "was this column renamed, or dropped and replaced?".
--
-- The question has been asked since the diff engine was written. A rename and a
-- drop-plus-add are indistinguishable from two schemas and are not remotely
-- equivalent — one keeps the data and the other destroys it — so the engine
-- proposes candidates and refuses to guess, and the gate holds any migration
-- with an unanswered one.
--
-- Nothing could answer. There was no table to record it in, no method to write
-- one, and no route to reach it, so a migration that raised a rename question
-- was held permanently: the gate said "N rename question(s) are unanswered" and
-- nothing in the product could ever make that number smaller.
--
-- Held against the change request rather than the migration, unlike approvals.
-- An approval is evidence about a particular list of statements and must expire
-- when that list is replaced. This is a statement about intent — that this
-- column became that one — and regenerating the plan does not make it less
-- true. It is re-checked against the schemas each time it is used, so an answer
-- that no longer describes anything simply stops applying.
CREATE TABLE schemaver.rename_decision (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_request_id bigint NOT NULL
        REFERENCES schemaver.change_request (id) ON DELETE CASCADE,

    -- The pair being ruled on, by name rather than by change id. Change ids
    -- carry the same names, and naming the columns means an answer survives a
    -- regeneration that renumbered or reclassified the changes around it.
    namespace   text NOT NULL,
    table_name  text NOT NULL,
    from_column text NOT NULL,
    to_column   text NOT NULL,

    -- true: the same column under a new name, and the data comes with it.
    -- false: two different columns, and the old one's data is discarded.
    --
    -- Both answers settle the question. Only one of them changes the
    -- statements; the other confirms that what was already going to happen is
    -- what was meant, which is the more dangerous of the two and therefore the
    -- one most worth having on the record.
    renamed boolean NOT NULL,

    decided_by bigint REFERENCES schemaver.app_user (id) ON DELETE SET NULL,
    decided_at timestamptz NOT NULL DEFAULT now(),
    note text,

    -- One answer per pair per request. Changing your mind updates it rather
    -- than stacking a second opinion behind the first.
    UNIQUE (change_request_id, namespace, table_name, from_column, to_column)
);
