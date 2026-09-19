package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/history"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// RequestSummary is one change request, as it appears in a list.
type RequestSummary struct {
	ID          int64
	Title       string
	State       string
	StateReason string
	Author      string
	Database    string
	Source      string
	CreatedAt   time.Time
	Changes     int
	OpenThreads int
	Approvals   int
}

// Requests lists change requests, newest first, with the ones still needing
// attention ahead of the ones that are finished.
func (s *Scope) Requests(ctx context.Context) ([]RequestSummary, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT r.id, r.title, r.state, COALESCE(r.state_reason, ''),
		       COALESCE(u.email, 'removed user'),
		       d.name, COALESCE(src.name, ''), r.created_at,
		       COALESCE((SELECT jsonb_array_length(m.changes) FROM schemaver.migration m
		                  WHERE m.change_request_id = r.id AND m.superseded_at IS NULL), 0),
		       (SELECT count(*) FROM schemaver.review_thread t
		         WHERE t.change_request_id = r.id AND t.status = 'open'),
		       (SELECT count(*) FROM schemaver.review_decision v
		          JOIN schemaver.migration m ON m.id = v.migration_id
		         WHERE v.change_request_id = r.id AND m.superseded_at IS NULL
		           AND v.decision = 'approve' AND v.reviewer_role = 'admin')
		  FROM schemaver.change_request r
		  JOIN schemaver.database d ON d.id = r.database_id
		  LEFT JOIN schemaver.database src ON src.id = r.source_database_id
		  LEFT JOIN schemaver.app_user u ON u.id = r.author_id
		 WHERE r.project_id = ANY($1)
		 ORDER BY (r.state IN ('COMPLETED', 'CLOSED')), r.created_at DESC`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list change requests: %w", err)
	}
	defer rows.Close()

	var out []RequestSummary
	for rows.Next() {
		var r RequestSummary
		if err := rows.Scan(&r.ID, &r.Title, &r.State, &r.StateReason, &r.Author,
			&r.Database, &r.Source, &r.CreatedAt, &r.Changes, &r.OpenThreads,
			&r.Approvals); err != nil {
			return nil, fmt.Errorf("scan change request: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Step is one statement of a stored migration.
type Step struct {
	Ordinal       int
	SQL           string
	ChangeID      string
	Transactional bool
	Note          string
}

// ErrNoSuchRequest is returned when a request is not there, or belongs to
// somebody else — the same answer either way, so the error cannot be used to
// find out which.
var ErrNoSuchRequest = errors.New("no such change request")

// RequestDetail is everything the review page shows.
type RequestDetail struct {
	RequestSummary
	Description string

	MigrationID        int64
	From, To           schema.Version
	IrreversibleReason string
	GeneratedAt        time.Time

	// Sizes is what each table on the target database costs to touch, keyed by
	// qualified name. Kept apart from the schema for the reason it is stored
	// apart: a table growing is not a schema change.
	Sizes map[string]TableSize

	// DatabaseID is the target, carried so the page can read what that database
	// costs without finding it again by name.
	DatabaseID int64

	// Objects is which tables, enums and sequences this change touches, and
	// ObjectSummary counts them. Asked before anything else — "what is this a
	// change to" — and answered badly by a list of statements.
	Objects       []history.ObjectChange
	ObjectSummary history.Summary

	// ByRisk is the change list ordered for review rather than for execution:
	// destructive first, metadata last. Execution order is a dependency
	// property; review order should follow what can hurt, or the one dangerous
	// change sits at position fourteen behind thirteen harmless ones.
	ByRisk  []diff.Change
	Renames []diff.RenameCandidate
	Steps   []Step

	Threads  []Thread
	Approval *ApprovalState
	// Executions is every attempt at this migration, newest first, empty if it
	// has never been run. Every attempt, because a run that was blocked and
	// rolled back is precisely what a viewer needs to see, and showing only the
	// last one hides it behind the retry that succeeded.
	Executions []*ExecutionView

	// Timeline is the request's own story — opened, generated, reviewed,
	// queued. What happened inside an execution belongs to that execution.
	Timeline []Activity

	// Targets are the databases this change reaches, in promotion order, and
	// how far it has got along them.
	Targets []RequestTarget

	// ClosedAt and ClosedBy record who settled this request and when.
	ClosedAt *time.Time
	ClosedBy string

	// MergeBase is where the two databases last agreed, set only when this
	// migration is a merge. Kept is what the target database did on its own
	// since then — work this change preserves rather than discards.
	//
	// Shown because otherwise the page is quietly confusing: the statements are
	// fewer than the difference between the two schemas, and the declared
	// target is a fingerprint neither database is at. A reviewer checking the
	// arithmetic would find it wrong and have nothing to explain it.
	MergeBase schema.Version
	Kept      []diff.Change

	// Branch names the branch this change was merged from, empty when it came
	// from another database. The page needs it because the other side of the
	// comparison is then not a database, and without it the reader is left
	// working out which one it was meant to be.
	Branch   string
	BranchID int64

	// AuthoredSQL is the script somebody wrote, where the change was written
	// rather than derived from another database. Kept so it can be corrected:
	// a script that would not apply produces no migration and therefore no
	// statements to edit, and without this the only way past a typo is to
	// abandon the request and write it again.
	AuthoredSQL string

	// WaitingSince is when the outstanding background work was queued, zero
	// where none is.
	//
	// Used to bound the page's self-refreshing. Whatever the reason a piece of
	// work does not finish — a worker that is gone, a precondition nobody
	// anticipated, a bug — a page that polls for it forever is worse than a
	// stale page, and the reader cannot be the one who notices.
	WaitingSince time.Time

	// RunBlocked is why a queued run cannot start, empty where nothing is
	// wrong.
	//
	// An execute job is claimable only while the plan's starting point still
	// matches the database and the plan is still the current one. That is
	// deliberate — a migration queued behind another waits instead of failing
	// its precondition and retrying — but it means a job can sit unclaimable
	// permanently, and "queued" then says the opposite of what is true.
	RunBlocked string

	// Deriving is a request whose statements are still being worked out.
	//
	// A written change is understood by applying it to a throwaway database and
	// reading the result, which is queued rather than done on the click. Until
	// it finishes there is no migration — and a page that cannot tell that from
	// "there was nothing to do" tells somebody their schemas already agree when
	// schemaver has simply not finished looking.
	Deriving bool
}

// Execution is the most recent attempt, or nil if there has never been one.
func (d *RequestDetail) Execution() *ExecutionView {
	if len(d.Executions) == 0 {
		return nil
	}
	return d.Executions[0]
}

// Prior is every attempt before the most recent one.
func (d *RequestDetail) Prior() []*ExecutionView {
	if len(d.Executions) < 2 {
		return nil
	}
	return d.Executions[1:]
}

// riskOrder ranks classes by how much attention they deserve.
var riskOrder = map[diff.Class]int{
	diff.Destructive:  0,
	diff.Rewriting:    1,
	diff.LockHeavy:    2,
	diff.Additive:     3,
	diff.MetadataOnly: 4,
}

// Request loads one change request in full.
func (s *Scope) Request(ctx context.Context, id int64) (*RequestDetail, error) {
	var d RequestDetail
	var changesJSON, renamesJSON []byte
	var irreversible *string
	var migrationID *int64
	var from, to, mergeBase *string
	var generatedAt *time.Time

	err := s.store.pool.QueryRow(ctx, `
		SELECT r.id, r.title, COALESCE(r.description, ''), r.state,
		       COALESCE(r.state_reason, ''), COALESCE(u.email, 'removed user'),
		       db.name, db.id, COALESCE(src.name, ''), r.created_at,
		       m.id, m.from_fingerprint, m.to_fingerprint, m.generated_at,
		       m.irreversible_reason, m.merge_base,
		       COALESCE(br.name, ''), COALESCE(r.branch_id, 0),
		       COALESCE(m.changes, '[]'::jsonb),
		       COALESCE(NULLIF(m.rename_candidates, 'null'::jsonb), '[]'::jsonb),
		       COALESCE(r.authored_sql, ''),
		       EXISTS (SELECT 1 FROM schemaver.job j
		                WHERE j.kind = 'derive' AND j.target_kind = 'request'
		                  AND j.target_id = r.id
		                  AND j.state IN ('pending', 'running')),
		       r.closed_at, COALESCE(cb.email, '')
		  FROM schemaver.change_request r
		  JOIN schemaver.database db ON db.id = r.database_id
		  LEFT JOIN schemaver.database src ON src.id = r.source_database_id
		  LEFT JOIN schemaver.app_user u ON u.id = r.author_id
		  LEFT JOIN schemaver.app_user cb ON cb.id = r.closed_by
		  LEFT JOIN schemaver.branch br ON br.id = r.branch_id
		  LEFT JOIN schemaver.migration m
		         ON m.change_request_id = r.id AND m.superseded_at IS NULL
		 WHERE r.id = $1 AND r.project_id = ANY($2)`, id, s.projects).
		Scan(&d.ID, &d.Title, &d.Description, &d.State, &d.StateReason, &d.Author,
			&d.Database, &d.DatabaseID, &d.Source, &d.CreatedAt,
			&migrationID, &from, &to, &generatedAt, &irreversible, &mergeBase,
			&d.Branch, &d.BranchID,
			&changesJSON, &renamesJSON, &d.AuthoredSQL, &d.Deriving,
			&d.ClosedAt, &d.ClosedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSuchRequest
	}
	if err != nil {
		return nil, fmt.Errorf("load change request %d: %w", id, err)
	}

	if migrationID != nil {
		d.MigrationID = *migrationID
		d.From, d.To = schema.Version(*from), schema.Version(*to)
		d.GeneratedAt = *generatedAt
		if irreversible != nil {
			d.IrreversibleReason = *irreversible
		}
		if sizes, err := s.Sizes(ctx, d.DatabaseID); err == nil {
			d.Sizes = sizes
		}
		// Derived from the two ends rather than folded out of ByRisk: the
		// change list says what happens, and which objects are involved is a
		// property of the schemas themselves.
		if before, err := s.Blob(ctx, d.From); err == nil && before != nil {
			if after, err := s.Blob(ctx, d.To); err == nil && after != nil {
				d.Objects = history.ObjectsChanged(before, after)
				d.ObjectSummary = history.Count(d.Objects)
			}
		}
		if mergeBase != nil {
			d.MergeBase = schema.Version(*mergeBase)
			// Worked out from the two schemas rather than stored, because both
			// are already kept and a stored copy could disagree with them.
			// Read failures are not fatal: the merge itself is recorded, and a
			// page that will not load is worse than one missing a summary.
			if base, err := s.Blob(ctx, d.MergeBase); err == nil && base != nil {
				if ours, err := s.Blob(ctx, d.From); err == nil && ours != nil {
					d.Kept = diff.Compute(base, ours).Changes
				}
			}
		}
		if err := json.Unmarshal(changesJSON, &d.ByRisk); err != nil {
			return nil, fmt.Errorf("decode changes: %w", err)
		}
		if err := json.Unmarshal(renamesJSON, &d.Renames); err != nil {
			return nil, fmt.Errorf("decode rename candidates: %w", err)
		}
		d.Changes = len(d.ByRisk)

		sort.SliceStable(d.ByRisk, func(i, j int) bool {
			return riskOrder[d.ByRisk[i].Class] < riskOrder[d.ByRisk[j].Class]
		})

		steps, err := s.store.pool.Query(ctx, `
			SELECT ordinal, sql, change_id, transactional, COALESCE(note, '')
			  FROM schemaver.migration_step
			 WHERE migration_id = $1 ORDER BY ordinal`, *migrationID)
		if err != nil {
			return nil, fmt.Errorf("load steps: %w", err)
		}
		for steps.Next() {
			var st Step
			if err := steps.Scan(&st.Ordinal, &st.SQL, &st.ChangeID,
				&st.Transactional, &st.Note); err != nil {
				steps.Close()
				return nil, fmt.Errorf("scan step: %w", err)
			}
			d.Steps = append(d.Steps, st)
		}
		steps.Close()
		if err := steps.Err(); err != nil {
			return nil, err
		}

		if state, err := s.ApprovalState(ctx, id); err == nil {
			d.Approval = state
		} else if !errors.Is(err, ErrNoMigration) {
			return nil, err
		}
	}

	d.Threads, err = s.Threads(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Executions, err = s.Executions(ctx, id); err != nil {
		return nil, err
	}

	// Only for a run that was asked for and has not started. Anything else
	// either has an execution to show or has not been queued, and asking would
	// be a query per page view for a question nobody has.
	if d.Queued() {
		if d.RunBlocked, err = s.runBlockage(ctx, d.MigrationID); err != nil {
			return nil, err
		}
	}

	// When the oldest outstanding piece of work for this request was queued,
	// whatever kind it is. One question rather than one per kind, because the
	// page only wants to know how long it has been waiting.
	var since *time.Time
	if err := s.store.pool.QueryRow(ctx, `
		SELECT min(j.created_at) FROM schemaver.job j
		 WHERE j.state IN ('pending', 'running')
		   AND ((j.target_kind = 'request' AND j.target_id = $1)
		     OR (j.target_kind = 'migration' AND j.target_id = $2))`,
		id, d.MigrationID).Scan(&since); err != nil {
		return nil, fmt.Errorf("check how long this has been waiting: %w", err)
	}
	if since != nil {
		d.WaitingSince = *since
	}
	if d.Timeline, err = s.ActivityForRequest(ctx, id); err != nil {
		return nil, err
	}
	if d.Targets, err = s.Targets(ctx, id); err != nil {
		return nil, err
	}
	for _, t := range d.Threads {
		if t.Status == "open" {
			d.OpenThreads++
		}
	}
	return &d, nil
}

// Candidates lists databases that could be brought in line with another, for the
// new-request form.
func (s *Scope) Candidates(ctx context.Context) ([]DatabaseRow, error) {
	all, err := s.Fleet(ctx)
	if err != nil {
		return nil, err
	}
	// Only databases with an observed schema can be diffed, and only ones still
	// being watched can be changed. Offering either of the others would produce
	// a request that is refused at the next step.
	var out []DatabaseRow
	for _, d := range all {
		if d.Fingerprint != "" && d.Writable {
			out = append(out, d)
		}
	}
	return out, nil
}

// ExecutionStep is one statement's run, for the progress view.
type ExecutionStep struct {
	Ordinal  int
	SQL      string
	ChangeID string
	Started  *time.Time
	Finished *time.Time
	Error    string
}

// Running reports a statement in flight.
func (s ExecutionStep) Running() bool { return s.Started != nil && s.Finished == nil }

// Elapsed reports how long the statement took, or has been going.
func (s ExecutionStep) Elapsed() time.Duration {
	if s.Started == nil {
		return 0
	}
	if s.Finished == nil {
		return time.Since(*s.Started)
	}
	return s.Finished.Sub(*s.Started)
}

// ExecutionView is a migration's progress, live or finished.
type ExecutionView struct {
	ID      int64
	State   string
	Total   int
	Done    int
	Started time.Time
	Ended   *time.Time
	Reason  string
	// Direction says whether this run applied the migration or undid it.
	Direction string
	// Final is the fingerprint the database ended at, typed as a version rather
	// than a string so it renders through the same shortener as every other
	// fingerprint instead of needing its own.
	Final schema.Version

	CurrentStep    *int
	CurrentStarted *time.Time

	WaitEvent    string
	BlockedBy    []int32
	BlockerQuery string
	Phase        string
	Percent      *float64
	ObservedAt   *time.Time

	Steps  []ExecutionStep
	Events []Activity
}

// Running reports whether this execution is still in flight, which is what
// decides whether the page should keep refreshing.
func (v *ExecutionView) Running() bool { return v.State == "running" }

// Blocked reports whether another session is standing in the way. The most
// useful thing to surface while a migration appears to hang.
func (v *ExecutionView) Blocked() bool { return len(v.BlockedBy) > 0 }

// Elapsed is how long the whole execution has taken so far.
func (v *ExecutionView) Elapsed() time.Duration {
	if v.Ended != nil {
		return v.Ended.Sub(v.Started)
	}
	return time.Since(v.Started)
}

// CurrentElapsed is how long the statement in flight has been running. A long
// index build and a hung statement look identical without this.
func (v *ExecutionView) CurrentElapsed() time.Duration {
	if v.CurrentStarted == nil {
		return 0
	}
	return time.Since(*v.CurrentStarted)
}

// Executions returns every attempt at a request's current migration, newest
// first.
//
// Three queries rather than three per attempt: the attempts, then their
// statements, then their events, each fanned out by execution in memory. A
// migration that was blocked, rolled back and retried is the normal case rather
// than the exception, so this path should not grow with the number of retries.
func (s *Scope) Executions(ctx context.Context, requestID int64) ([]*ExecutionView, error) {
	if err := s.ownsRequest(ctx, requestID); err != nil {
		return nil, err
	}

	rows, err := s.store.pool.Query(ctx, `
		SELECT e.id, e.state, e.statements_total, e.statements_done,
		       e.started_at, e.finished_at, COALESCE(e.reason, ''),
		       e.direction, COALESCE(e.final_fingerprint, ''),
		       e.current_step, e.current_started_at,
		       COALESCE(e.wait_event, ''), e.blocked_by,
		       COALESCE(e.blocker_query, ''),
		       COALESCE(e.progress_phase, ''), e.progress_percent, e.observed_at
		  FROM schemaver.execution e
		  JOIN schemaver.migration m ON m.id = e.migration_id
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL
		 ORDER BY e.started_at DESC, e.id DESC`, requestID)
	if err != nil {
		return nil, fmt.Errorf("load executions: %w", err)
	}
	defer rows.Close()

	var views []*ExecutionView
	byID := map[int64]*ExecutionView{}
	var ids []int64
	for rows.Next() {
		var v ExecutionView
		// Scanned as a string and converted, the way From and To are: the
		// driver is not asked to know about our named types.
		var final string
		if err := rows.Scan(&v.ID, &v.State, &v.Total, &v.Done,
			&v.Started, &v.Ended, &v.Reason, &v.Direction, &final,
			&v.CurrentStep, &v.CurrentStarted,
			&v.WaitEvent, &v.BlockedBy, &v.BlockerQuery,
			&v.Phase, &v.Percent, &v.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan execution: %w", err)
		}
		v.Final = schema.Version(final)
		views = append(views, &v)
		byID[v.ID] = &v
		ids = append(ids, v.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, nil
	}

	// Joined against the statements themselves so the view shows what ran, not
	// merely how many.
	steps, err := s.store.pool.Query(ctx, `
		SELECT e.id, st.ordinal, st.sql, st.change_id,
		       es.started_at, es.finished_at, COALESCE(es.error, '')
		  FROM schemaver.execution e
		  JOIN schemaver.migration_step st ON st.migration_id = e.migration_id
		  LEFT JOIN schemaver.execution_step es
		         ON es.execution_id = e.id AND es.ordinal = st.ordinal
		 WHERE e.id = ANY($1)
		 ORDER BY e.id, st.ordinal`, ids)
	if err != nil {
		return nil, fmt.Errorf("load execution steps: %w", err)
	}
	defer steps.Close()
	for steps.Next() {
		var id int64
		var st ExecutionStep
		if err := steps.Scan(&id, &st.Ordinal, &st.SQL, &st.ChangeID,
			&st.Started, &st.Finished, &st.Error); err != nil {
			return nil, fmt.Errorf("scan execution step: %w", err)
		}
		if v := byID[id]; v != nil {
			v.Steps = append(v.Steps, st)
		}
	}
	if err := steps.Err(); err != nil {
		return nil, err
	}

	events, err := s.store.pool.Query(ctx, `
		SELECT a.execution_id,`+activityColumns+`
		  FROM schemaver.activity a
		  LEFT JOIN schemaver.database d ON d.id = a.database_id
		  LEFT JOIN schemaver.change_request r ON r.id = a.request_id
		 WHERE a.execution_id = ANY($1)
		 ORDER BY a.execution_id, a.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("load execution events: %w", err)
	}
	defer events.Close()
	for events.Next() {
		var id int64
		var a Activity
		if err := events.Scan(&id, &a.ID, &a.At, &a.Actor, &a.Level, &a.Kind,
			&a.Message, &a.Detail, &a.DatabaseID, &a.RequestID, &a.ExecutionID,
			&a.Ordinal, &a.Database, &a.Request); err != nil {
			return nil, fmt.Errorf("scan execution event: %w", err)
		}
		if v := byID[id]; v != nil {
			v.Events = append(v.Events, a)
		}
	}
	return views, events.Err()
}

// Ran reports that this change reached a database, so the ending it deserves is
// "done" rather than "closed".
func (d *RequestDetail) Ran() bool {
	return d.State == "COMPLETED" || d.State == "NEEDS_ATTENTION" ||
		d.State == "FAILED" || len(d.Executions) > 0
}

// Pipeline reports that this change reaches more than one database.
func (d *RequestDetail) Pipeline() bool { return len(d.Targets) > 1 }

// runBlockage reports why a queued execution cannot be claimed.
//
// The conditions mirror the claim query exactly. They are asked separately
// rather than inferred, because each has a different answer for the person
// reading: a superseded plan needs the new one running, a moved database needs
// the plan rebuilt, and a retired database needs neither.
func (s *Scope) runBlockage(ctx context.Context, migrationID int64) (string, error) {
	var superseded, retired bool
	var dbName string
	var migFrom, dbNow *string
	err := s.store.pool.QueryRow(ctx, `
		SELECT m.superseded_at IS NOT NULL, d.retired_at IS NOT NULL,
		       COALESCE(d.name, ''), m.from_fingerprint, d.current_fingerprint
		  FROM schemaver.job j
		  JOIN schemaver.migration m ON m.id = j.target_id
		  LEFT JOIN schemaver.database d ON d.id = j.database_id
		 WHERE j.kind = 'execute' AND j.target_kind = 'migration'
		   AND j.target_id = $1
		   AND j.state IN ('pending', 'running')
		 ORDER BY j.id DESC LIMIT 1`, migrationID).
		Scan(&superseded, &retired, &dbName, &migFrom, &dbNow)
	if errors.Is(err, pgx.ErrNoRows) {
		// No job at all. The request says it is ready to run and nothing was
		// ever queued, which is its own kind of stuck.
		return "nothing is queued for this migration, so pressing run again is " +
			"what it needs", nil
	}
	if err != nil {
		return "", fmt.Errorf("check why the run has not started: %w", err)
	}
	switch {
	case superseded:
		return "this plan has been replaced since the run was queued, so the " +
			"queued one will never start; run the current plan instead", nil
	case retired:
		return dbName + " has been retired, so nothing will be applied to it", nil
	case dbNow == nil:
		return dbName + " has not been read successfully, so there is nothing " +
			"to check this plan starts from", nil
	case migFrom != nil && *dbNow != *migFrom:
		return dbName + " has moved since this plan was made — the plan starts " +
			"from " + schema.Version(*migFrom).Short() + " and the database is " +
			"at " + schema.Version(*dbNow).Short() + ". Rebuild the plan and it " +
			"becomes runnable again", nil
	}
	return "", nil
}

// Queued reports a run that has been asked for and not yet started.
//
// Pressing execute records the request as ready and queues the work; a worker
// claims it a moment later. In between there is no execution to show, so a page
// that only knows about running executions is identical to the page before the
// press — which reads as the press having done nothing.
func (d *RequestDetail) Queued() bool {
	return d.State == "READY_TO_EXECUTE" && d.Execution() == nil
}

// Stuck reports a queued run that cannot start. Nothing is going to happen, so
// the page has no reason to keep reloading.
func (d *RequestDetail) Stuck() bool { return d.Queued() && d.RunBlocked != "" }

// patience is how long the page will follow a piece of work before giving up on
// it.
//
// Generous against the slowest thing it waits for — a rehearsal of a large
// schema — and far short of forever. Every specific reason work might never
// finish is worth diagnosing, and this is the backstop for the ones nobody
// thought of.
const patience = 3 * time.Minute

// Overdue reports work that has been outstanding longer than the page is
// willing to follow.
func (d *RequestDetail) Overdue() bool {
	return !d.WaitingSince.IsZero() && time.Since(d.WaitingSince) > patience
}

// Working reports that schemaver owes this request something that finishes on
// its own: a derivation, a rehearsal, or a run. It is what decides whether the
// page keeps itself up to date.
//
// Waiting on a person is deliberately not working. A page that reloads every
// few seconds while somebody types a comment throws the comment away.
func (d *RequestDetail) Working() bool {
	// Whatever is outstanding, the page stops following it eventually. This is
	// the one check that does not depend on having understood why a particular
	// piece of work might never land.
	if d.Overdue() {
		return false
	}
	if d.Deriving {
		return true
	}
	if d.Queued() {
		// Unless it cannot start. Reloading every few seconds forever, waiting
		// for something that will not happen, is worse than standing still.
		return !d.Stuck()
	}
	if x := d.Execution(); x != nil && x.Running() {
		return true
	}
	return d.Approval != nil && d.Approval.ProofState == "pending"
}

// NextTarget is the database a press of the execute button would run against,
// or nil when every target has the change.
func (d *RequestDetail) NextTarget() *RequestTarget {
	for i := range d.Targets {
		if !d.Targets[i].Reached {
			return &d.Targets[i]
		}
	}
	return nil
}
