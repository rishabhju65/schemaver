package store

import (
	"context"
	"errors"
	"fmt"
)

// writableDatabase is the predicate every write path applies before letting a
// change be aimed at a database. Written once and referenced everywhere, so the
// four conditions cannot drift apart between the places that check them.
//
// It expects the database aliased `d` and its instance aliased `i`.
//
//   - retired: somebody stood it down deliberately.
//   - archived: discovery found it gone from the server.
//   - unmanaged: we are not watching it, so any fingerprint we hold is of
//     unknown age — and a migration is a promise about a starting state.
//   - the instance itself archived: the same argument, one level up.
//
// Reads are deliberately not subject to this. The whole point of retiring
// rather than deleting is that the history stays legible.
//
// Parenthesised as part of the constant so that it can be dropped into a
// SELECT list as well as a WHERE clause, and so the literals it is spliced
// between stay balanced on their own.
const writableDatabase = `(
	d.retired_at IS NULL
	AND d.archived_at IS NULL
	AND d.managed
	AND i.archived_at IS NULL)`

// ErrNotWritable is returned when a change is aimed at a database that is no
// longer somewhere changes can be sent.
var ErrNotWritable = errors.New(
	"this database is retired, no longer present on its server, or not being managed; " +
		"its history stays readable but no new changes can be made against it")

// requireWritable reports whether every named database can still be changed.
//
// One query for all of them rather than one each: the caller usually has a pair
// — a target and the source it is being brought in line with — and a partial
// answer is not useful.
func (s *Scope) requireWritable(ctx context.Context, databaseIDs ...int64) error {
	if len(databaseIDs) == 0 {
		return nil
	}
	var writable int
	if err := s.store.pool.QueryRow(ctx, `
		SELECT count(DISTINCT d.id) FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = ANY($1) AND i.project_id = ANY($2)
		   AND`+writableDatabase, databaseIDs, s.projects).Scan(&writable); err != nil {
		return fmt.Errorf("check databases are writable: %w", err)
	}
	if writable != distinct(databaseIDs) {
		return ErrNotWritable
	}
	return nil
}

// distinct counts unique ids, so passing the same database twice does not make
// the comparison above fail.
func distinct(ids []int64) int {
	seen := map[int64]struct{}{}
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return len(seen)
}

// requireWritableForRequest checks the database a change request is aimed at.
//
// Separate from requireWritable because the caller holds a request rather than
// a database, and resolving one to the other in every caller is how a check
// ends up being skipped in the one place it mattered.
func (s *Scope) requireWritableForRequest(ctx context.Context, requestID int64) error {
	var writable bool
	err := s.store.pool.QueryRow(ctx, `
		SELECT`+writableDatabase+`
		  FROM schemaver.change_request r
		  JOIN schemaver.database d ON d.id = r.database_id
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE r.id = $1 AND i.project_id = ANY($2)`,
		requestID, s.projects).Scan(&writable)
	if err != nil {
		return fmt.Errorf("check the request's database is writable: %w", err)
	}
	if !writable {
		return ErrNotWritable
	}
	return nil
}
