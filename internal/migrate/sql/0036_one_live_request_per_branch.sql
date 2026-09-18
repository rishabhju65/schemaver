-- One live change request per branch per database.
--
-- Nothing stopped a branch being merged twice into the same database, so
-- pressing the button again — or having two tabs open, or going back — opened a
-- second request carrying the same statements. Both sat in review looking real,
-- both were approvable, and the second only became visibly wrong after the
-- first one ran.
--
-- The application refuses it now with a message naming the request that already
-- exists. This index is what makes it true rather than likely: two concurrent
-- merges can both pass a check and only one can win an index.
--
-- "Live" means still in play. A settled request does not block anything, and
-- neither does a completed one: a branch that has moved on since it last landed
-- is entitled to land again.

-- Existing duplicates first, because an index cannot be built over them.
--
-- The oldest live request per branch and database is the one somebody has been
-- reviewing, so it is the one that stays. The rest are closed with a reason
-- that says what happened, rather than deleted — they were real requests that
-- real people may have looked at.
WITH ranked AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY branch_id, database_id
               ORDER BY created_at, id
           ) AS n
      FROM schemaver.change_request
     WHERE branch_id IS NOT NULL
       AND state NOT IN ('CLOSED', 'DONE', 'REVERTED', 'COMPLETED')
)
UPDATE schemaver.change_request r
   SET state = 'CLOSED',
       state_reason = 'closed as a duplicate: another request was already open '
                      'for this branch against this database',
       closed_at = now(),
       updated_at = now()
  FROM ranked
 WHERE r.id = ranked.id AND ranked.n > 1;

CREATE UNIQUE INDEX change_request_one_live_per_branch_idx
    ON schemaver.change_request (branch_id, database_id)
 WHERE branch_id IS NOT NULL
   AND state NOT IN ('CLOSED', 'DONE', 'REVERTED', 'COMPLETED');

COMMENT ON INDEX schemaver.change_request_one_live_per_branch_idx IS
    'A branch may have one request in play against a database at a time. '
    'Settled and completed requests do not block a new one, because a branch '
    'that has moved on since it landed is entitled to land again.';
