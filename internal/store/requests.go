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
	// Execution is the most recent run of this migration, live or finished, or
	// nil if it has never been executed.
	Execution *ExecutionView
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
	if d.Execution, err = s.LatestExecution(ctx, id); err != nil {
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
	// Only databases with an observed schema can be diffed; offering the others
	// would produce a request that cannot generate anything.
	var out []DatabaseRow
	for _, d := range all {
		if d.Fingerprint != "" {
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
	Final   string

	CurrentStep    *int
	CurrentStarted *time.Time

	WaitEvent    string
	BlockedBy    []int32
	BlockerQuery string
	Phase        string
	Percent      *float64
	ObservedAt   *time.Time

	Steps []ExecutionStep
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

// LatestExecution returns the most recent execution of a request's current
// migration, or nil if it has never been run.
func (s *Scope) LatestExecution(ctx context.Context, requestID int64) (*ExecutionView, error) {
	if err := s.ownsRequest(ctx, requestID); err != nil {
		return nil, err
	}

	var v ExecutionView
	var reason, final, wait, blocker, phase *string
	err := s.store.pool.QueryRow(ctx, `
		SELECT e.id, e.state, e.statements_total, e.statements_done,
		       e.started_at, e.finished_at, e.reason, e.final_fingerprint,
		       e.current_step, e.current_started_at,
		       e.wait_event, e.blocked_by, e.blocker_query,
		       e.progress_phase, e.progress_percent, e.observed_at
		  FROM schemaver.execution e
		  JOIN schemaver.migration m ON m.id = e.migration_id
		 WHERE m.change_request_id = $1 AND m.superseded_at IS NULL
		 ORDER BY e.started_at DESC LIMIT 1`, requestID).
		Scan(&v.ID, &v.State, &v.Total, &v.Done, &v.Started, &v.Ended,
			&reason, &final, &v.CurrentStep, &v.CurrentStarted,
			&wait, &v.BlockedBy, &blocker, &phase, &v.Percent, &v.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load execution: %w", err)
	}
	for target, src := range map[*string]*string{
		&v.Reason: reason, &v.Final: final, &v.WaitEvent: wait,
		&v.BlockerQuery: blocker, &v.Phase: phase,
	} {
		if src != nil {
			*target = *src
		}
	}

	// Joined against the statements themselves so the view shows what ran, not
	// merely how many.
	rows, err := s.store.pool.Query(ctx, `
		SELECT st.ordinal, st.sql, st.change_id,
		       es.started_at, es.finished_at, COALESCE(es.error, '')
		  FROM schemaver.migration_step st
		  JOIN schemaver.execution e ON e.id = $1
		  LEFT JOIN schemaver.execution_step es
		         ON es.execution_id = e.id AND es.ordinal = st.ordinal
		 WHERE st.migration_id = e.migration_id
		 ORDER BY st.ordinal`, v.ID)
	if err != nil {
		return nil, fmt.Errorf("load execution steps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st ExecutionStep
		if err := rows.Scan(&st.Ordinal, &st.SQL, &st.ChangeID,
			&st.Started, &st.Finished, &st.Error); err != nil {
			return nil, fmt.Errorf("scan execution step: %w", err)
		}
		v.Steps = append(v.Steps, st)
	}
	return &v, rows.Err()
}
