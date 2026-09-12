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

// RequestDetail is everything the review page shows.
type RequestDetail struct {
	RequestSummary
	Description string

	MigrationID        int64
	From, To           schema.Version
	IrreversibleReason string
	GeneratedAt        time.Time

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

	// Revert is the generated way back, read at approval time so the cost of
	// undoing is known before the change runs rather than during the incident
	// (D-012). Nothing executes it.
	Revert []RevertStep
}

// RevertLosesData reports that undoing would not restore everything, so the
// page can say so once at the top rather than only per statement.
func (d *RequestDetail) RevertLosesData() bool {
	for _, st := range d.Revert {
		if st.StructureOnly {
			return true
		}
	}
	return false
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
	var from, to *string
	var generatedAt *time.Time

	err := s.store.pool.QueryRow(ctx, `
		SELECT r.id, r.title, COALESCE(r.description, ''), r.state,
		       COALESCE(r.state_reason, ''), COALESCE(u.email, 'removed user'),
		       db.name, COALESCE(src.name, ''), r.created_at,
		       m.id, m.from_fingerprint, m.to_fingerprint, m.generated_at,
		       m.irreversible_reason,
		       COALESCE(m.changes, '[]'::jsonb),
		       COALESCE(NULLIF(m.rename_candidates, 'null'::jsonb), '[]'::jsonb)
		  FROM schemaver.change_request r
		  JOIN schemaver.database db ON db.id = r.database_id
		  LEFT JOIN schemaver.database src ON src.id = r.source_database_id
		  LEFT JOIN schemaver.app_user u ON u.id = r.author_id
		  LEFT JOIN schemaver.migration m
		         ON m.change_request_id = r.id AND m.superseded_at IS NULL
		 WHERE r.id = $1 AND r.project_id = ANY($2)`, id, s.projects).
		Scan(&d.ID, &d.Title, &d.Description, &d.State, &d.StateReason, &d.Author,
			&d.Database, &d.Source, &d.CreatedAt,
			&migrationID, &from, &to, &generatedAt, &irreversible,
			&changesJSON, &renamesJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("no such change request")
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
	if d.Timeline, err = s.ActivityForRequest(ctx, id); err != nil {
		return nil, err
	}
	if d.MigrationID != 0 {
		if d.Revert, err = s.RevertSteps(ctx, d.MigrationID); err != nil {
			return nil, err
		}
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
		       COALESCE(e.final_fingerprint, ''),
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
			&v.Started, &v.Ended, &v.Reason, &final,
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
