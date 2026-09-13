-- Where a change request came from, when it came from a branch.
--
-- A branch merge is an ordinary change request and deliberately so: it is
-- reviewed, approved, rehearsed against a throwaway copy, promoted through the
-- environments below and rolled back by exactly the machinery that already
-- exists, none of which needs to know a branch was involved. What the *page*
-- needs to know is that the other side of this comparison is not a database, so
-- it can name the branch rather than leaving the reader wondering which
-- database `source_database_id` was supposed to be.
--
-- ON DELETE SET NULL: closing or removing a branch must not take the record of
-- what was merged from it with it.
ALTER TABLE schemaver.change_request
    ADD COLUMN branch_id bigint
        REFERENCES schemaver.branch (id) ON DELETE SET NULL;
