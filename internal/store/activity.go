package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Event is one entry in the activity log, before it is written.
//
// Built by the three severity constructors and narrowed with the On* methods,
// so the level is a property of how the event is made rather than a string each
// caller has to remember, and the entity tags cannot be passed in the wrong
// positional order.
type Event struct {
	Level   string
	Kind    string
	Message string
	Detail  any

	ActorID    *int64
	ActorLabel string

	DatabaseID  *int64
	InstanceID  *int64
	RequestID   *int64
	MigrationID *int64
	ExecutionID *int64
	Ordinal     *int
}

// Info, Warn and Error start an event at each severity.
//
// Every builder below takes and returns a pointer, so they chain in any order.
// Mixing value and pointer receivers would make the order significant, which is
// exactly the kind of trap a fluent interface should not contain.
func Info(kind, message string) *Event  { return &Event{Level: "info", Kind: kind, Message: message} }
func Warn(kind, message string) *Event  { return &Event{Level: "warn", Kind: kind, Message: message} }
func Error(kind, message string) *Event { return &Event{Level: "error", Kind: kind, Message: message} }

// By attributes the event to a person. Left unset, it reads as schemaver
// itself, which is the truth for anything a worker does on its own.
//
// The label is looked up at write time if it is not given, so callers deep in
// the store do not have to carry an email through their signatures for the sake
// of one log line.
func (e *Event) By(userID int64) *Event {
	e.ActorID = &userID
	return e
}

// OnDatabase, OnInstance, OnRequest, OnMigration and OnExecution tag the
// entities the event concerns. An event usually carries several: tagging it
// with all of them is what makes every timeline a single indexed read.
func (e *Event) OnDatabase(id int64) *Event  { e.DatabaseID = &id; return e }
func (e *Event) OnInstance(id int64) *Event  { e.InstanceID = &id; return e }
func (e *Event) OnRequest(id int64) *Event   { e.RequestID = &id; return e }
func (e *Event) OnMigration(id int64) *Event { e.MigrationID = &id; return e }
func (e *Event) OnExecution(id int64) *Event { e.ExecutionID = &id; return e }

// At names the statement within an execution.
func (e *Event) At(ordinal int) *Event { e.Ordinal = &ordinal; return e }

// With attaches the structured form of the same thing.
func (e *Event) With(detail any) *Event { e.Detail = detail; return e }

// dsnPassword matches the credentials in a connection string. Connection errors
// routinely quote the string that failed, password included.
var dsnPassword = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/@\s]+:)[^@\s]*(@)`)

// passwordField matches a password given as a keyword, the other form a driver
// echoes back.
var passwordField = regexp.MustCompile(`(?i)\b(password)\s*=\s*(?:'[^']*'|"[^"]*"|\S+)`)

// redact removes credentials from anything on its way into the log.
//
// Applied here rather than at the call sites, because this log is kept
// indefinitely and a single caller forgetting would put a password somewhere
// nothing ever deletes. The two forms that actually occur are a connection
// string quoted back by a failed connection, and a password= keyword echoed by
// the driver; both arrive inside error text that no one composed deliberately,
// which is exactly why the boundary cannot be the caller's responsibility.
//
// Query text is a different problem and not one redaction can solve — a
// blocking statement's WHERE clause may hold real customer data, and there is
// no pattern that distinguishes it from a table name. That is a reason to think
// about what is captured, not something to strip here.
func redact(s string) string {
	s = dsnPassword.ReplaceAllString(s, "${1}[redacted]${2}")
	return passwordField.ReplaceAllString(s, "${1}=[redacted]")
}

// Record writes one entry to the activity log.
//
// Best-effort by contract: callers log the error and carry on. Failing to
// describe something must never fail the thing itself, which is why this is
// deliberately not an audit log — see 0009_activity.sql.
func (s *Store) Record(ctx context.Context, projectID int64, e *Event) error {
	var detail []byte
	if e.Detail != nil {
		if b, err := json.Marshal(e.Detail); err == nil {
			detail = b
		}
		// A detail that will not marshal is dropped rather than failing the
		// entry: the message is the half a person reads.
	}
	if e.Level == "" {
		e.Level = "info"
	}
	if e.ActorLabel == "" {
		e.ActorLabel = "schemaver"
		if e.ActorID != nil {
			// Denormalized at write time so the entry still reads as theirs
			// after the account is gone.
			var email string
			if err := s.pool.QueryRow(ctx,
				`SELECT email FROM schemaver.app_user WHERE id = $1`,
				*e.ActorID).Scan(&email); err == nil {
				e.ActorLabel = email
			}
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.activity
		       (project_id, actor_id, actor_label, level, kind, message, detail,
		        database_id, instance_id, request_id, migration_id,
		        execution_id, ordinal)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		projectID, e.ActorID, e.ActorLabel, e.Level, e.Kind,
		redact(e.Message), detail,
		e.DatabaseID, e.InstanceID, e.RequestID, e.MigrationID,
		e.ExecutionID, e.Ordinal)
	if err != nil {
		return fmt.Errorf("record activity: %w", err)
	}
	return nil
}

// Activity is one entry as read back.
type Activity struct {
	ID      int64
	At      time.Time
	Actor   string
	Level   string
	Kind    string
	Message string
	Detail  string

	DatabaseID  *int64
	RequestID   *int64
	ExecutionID *int64
	Ordinal     *int

	// Database and Request are the names behind those ids, resolved for display
	// so a feed reads as sentences rather than as numbers.
	Database string
	Request  string
}

// Notable reports an entry worth surfacing without reading the whole log.
func (a Activity) Notable() bool { return a.Level != "info" }

const activityColumns = `
	a.id, a.at, a.actor_label, a.level, a.kind, a.message,
	COALESCE(a.detail::text, ''),
	a.database_id, a.request_id, a.execution_id, a.ordinal,
	COALESCE(d.name, ''), COALESCE(r.title, '')`

// scanActivity reads the rows of any query selecting activityColumns.
func scanActivity(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]Activity, error) {
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.At, &a.Actor, &a.Level, &a.Kind, &a.Message,
			&a.Detail, &a.DatabaseID, &a.RequestID, &a.ExecutionID, &a.Ordinal,
			&a.Database, &a.Request); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivityForRequest returns the request's own story, oldest first: opened,
// generated, reviewed, queued.
//
// Entries belonging to an execution are left out, because each attempt shows
// its own log beside its own state and a reader should not meet the same lines
// twice on one page. A flat timeline could carry them, but then every line
// would need to say which attempt it came from, which is exactly the grouping
// the panels already provide.
func (s *Scope) ActivityForRequest(ctx context.Context, requestID int64) ([]Activity, error) {
	if err := s.ownsRequest(ctx, requestID); err != nil {
		return nil, err
	}
	rows, err := s.store.pool.Query(ctx, `
		SELECT`+activityColumns+`
		  FROM schemaver.activity a
		  LEFT JOIN schemaver.database d ON d.id = a.database_id
		  LEFT JOIN schemaver.change_request r ON r.id = a.request_id
		 WHERE a.request_id = $1 AND a.execution_id IS NULL
		 ORDER BY a.id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("load request activity: %w", err)
	}
	return scanActivity(rows)
}

// ActivityFeed returns the most recent activity across every project the caller
// may read, newest first.
//
// The one place the whole system's behaviour is visible at once: proposals,
// approvals, migrations running, databases drifting, in the order they
// happened.
func (s *Scope) ActivityFeed(ctx context.Context, limit int, level string) ([]Activity, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.store.pool.Query(ctx, `
		SELECT`+activityColumns+`
		  FROM schemaver.activity a
		  LEFT JOIN schemaver.database d ON d.id = a.database_id
		  LEFT JOIN schemaver.change_request r ON r.id = a.request_id
		 WHERE a.project_id = ANY($1)
		   AND ($2 = '' OR a.level = $2)
		 ORDER BY a.id DESC
		 LIMIT $3`, s.projects, level, limit)
	if err != nil {
		return nil, fmt.Errorf("load activity feed: %w", err)
	}
	return scanActivity(rows)
}

// record writes an entry against the scope's project and swallows the error.
//
// Every caller of this would otherwise write the same three lines to log a
// failure it cannot act on, and the risk of that boilerplate is that someone
// eventually returns the error instead — turning a missing log line into a
// failed proposal.
func (s *Scope) record(ctx context.Context, e *Event) {
	if s.writable == 0 {
		return
	}
	// The error is dropped rather than logged, because the store has no logger
	// and the only way this fails is the metadata database being unreachable —
	// in which case the write this describes has already failed and the caller
	// is returning that instead.
	_ = s.store.Record(ctx, s.writable, e)
}

// recordFor writes an entry against whichever project owns a database.
//
// The worker's half of the log goes through here. It holds no scope — a worker
// serves every project at once — so the project is resolved from the database
// the work concerns rather than carried in from a caller who does not have one.
func (s *Store) recordFor(ctx context.Context, databaseID int64, e *Event) {
	var projectID int64
	if err := s.pool.QueryRow(ctx, `
		SELECT i.project_id
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1`, databaseID).Scan(&projectID); err != nil {
		return
	}
	_ = s.Record(ctx, projectID, e)
}
