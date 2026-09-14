package store

import (
	"context"
	"fmt"
	"time"

	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/history"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// ObjectEvent is one moment a table, enum or sequence changed.
type ObjectEvent struct {
	At       time.Time
	From, To schema.Version

	// Kind is what happened to the object itself — it appeared, it went, or it
	// changed inside. Changes is what changed inside it, empty for the first
	// two.
	Kind    history.Kind
	Changes []diff.Change

	// RequestID and Title name the change request that caused this, where one
	// did. Zero means nobody asked for it through this product — somebody
	// changed the database directly, which is the thing worth being able to
	// see.
	RequestID int64
	Title     string
}

// Caused reports whether this transition came from a change request.
func (e ObjectEvent) Caused() bool { return e.RequestID != 0 }

// ObjectHistory is what has happened to one object on one database.
//
// Derived rather than stored, and that is not a shortcut. A snapshot holds a
// whole schema, so the history of any one object inside it is already recorded;
// what was missing was a way to ask. Git works the same way — there is no
// per-file history object anywhere in a repository, and `git log -- path` walks
// the commits and keeps the ones whose diff touches that path.
//
// It also answers two things a file history cannot. A version-controlled schema
// file tells you somebody edited a table; it does not tell you which migration
// carried the edit, or which database it reached. Both are here because both
// were already recorded against the transition.
func (s *Scope) ObjectHistory(ctx context.Context, databaseID int64, object string) ([]ObjectEvent, error) {
	if object == "" {
		return nil, fmt.Errorf("which object?")
	}
	entries, err := s.Timeline(ctx, TimelineFilter{DatabaseID: databaseID, Limit: 200})
	if err != nil {
		return nil, err
	}

	// Which change request spans each fingerprint pair. Read in one query
	// rather than per entry: a database with a long history would otherwise
	// make this page one round trip per snapshot.
	type span struct {
		id    int64
		title string
	}
	caused := map[string]span{}
	rows, err := s.store.pool.Query(ctx, `
		SELECT m.from_fingerprint, m.to_fingerprint, r.id, r.title
		  FROM schemaver.migration m
		  JOIN schemaver.change_request r ON r.id = m.change_request_id
		 WHERE r.project_id = ANY($1)`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("read what caused each change: %w", err)
	}
	for rows.Next() {
		var from, to, title string
		var id int64
		if err := rows.Scan(&from, &to, &id, &title); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cause: %w", err)
		}
		caused[from+">"+to] = span{id: id, title: title}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []ObjectEvent
	for _, e := range entries {
		if e.Failed() || e.From == "" || e.To == "" {
			continue
		}
		before, err := s.Blob(ctx, e.From)
		if err != nil || before == nil {
			continue
		}
		after, err := s.Blob(ctx, e.To)
		if err != nil || after == nil {
			continue
		}

		// Did this object change at all in this transition? The object-level
		// answer decides whether the entry belongs here; the change list says
		// what happened inside it.
		var kind history.Kind
		var touched bool
		for _, oc := range history.ObjectsChanged(before, after) {
			if oc.Name == object {
				kind, touched = oc.Kind, true
				break
			}
		}
		if !touched {
			continue
		}

		ev := ObjectEvent{At: e.ObservedAt, From: e.From, To: e.To, Kind: kind}
		ev.Changes = Delta{Changes: diff.Compute(before, after).Changes}.ObjectsIn(object)
		if c, ok := caused[string(e.From)+">"+string(e.To)]; ok {
			ev.RequestID, ev.Title = c.id, c.title
		}
		out = append(out, ev)
	}
	return out, nil
}

// Objects lists what a database currently holds, so a reader has something to
// pick from rather than having to know a name to type.
func (s *Scope) Objects(ctx context.Context, databaseID int64) ([]history.ObjectChange, error) {
	var fingerprint string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT COALESCE(d.current_fingerprint, '')
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		 WHERE d.id = $1 AND i.project_id = ANY($2)`, databaseID, s.projects).
		Scan(&fingerprint); err != nil {
		return nil, fmt.Errorf("read the database: %w", err)
	}
	if fingerprint == "" {
		return nil, nil
	}
	current, err := s.Blob(ctx, schema.Version(fingerprint))
	if err != nil || current == nil {
		return nil, err
	}
	// Against an empty schema, so everything reads as "added" — which is the
	// list of what is there, phrased as the diff engine phrases everything.
	return history.ObjectsChanged(&schema.Schema{}, current), nil
}
