package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/store"
)

func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := scope.Requests(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "requests", "Changes", "requests", map[string]any{"Rows": rows})
}

// requestNew proposes bringing one database in line with another, and generates
// the migration immediately.
//
// Generating at once rather than on a later click means the author sees what
// their request actually does before anyone else is asked to look at it — and a
// request that cannot generate anything never reaches review.
func (s *Server) requestNew(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	candidates, err := scope.Candidates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := func(cause error) map[string]any {
		m := map[string]any{"Databases": candidates}
		if cause != nil {
			m["Error"] = cause.Error()
		}
		return m
	}

	if r.Method == http.MethodGet {
		s.render(w, r, "request_new", "Propose a change", "requests", data(nil))
		return
	}

	user := userFrom(r.Context())
	target, terr := strconv.ParseInt(r.FormValue("database"), 10, 64)
	source, serr := strconv.ParseInt(r.FormValue("source"), 10, 64)
	if terr != nil || serr != nil {
		s.render(w, r, "request_new", "Propose a change", "requests",
			data(errors.New("choose a database to change and one to match")))
		return
	}

	id, err := scope.Propose(r.Context(), user.ID, target, source,
		strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("description")))
	if err != nil {
		s.render(w, r, "request_new", "Propose a change", "requests", data(err))
		return
	}
	if _, err := scope.GenerateMigration(r.Context(), user.ID, id); err != nil {
		// The request exists; generation failed. Show it rather than hiding the
		// request, so the author can see the reason on the page it belongs to.
		http.Redirect(w, r, "/requests/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/requests/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a request id", http.StatusBadRequest)
		return
	}
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	detail, err := scope.Request(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	user := userFrom(r.Context())
	s.render(w, r, "request", detail.Title, "requests", map[string]any{
		"R": detail,
		// Only while something is actually in flight.
		"Refresh": detail.Execution() != nil && detail.Execution().Running(),
		// The author cannot approve their own request unless they are the only
		// administrator, so the button is hidden rather than offered and refused.
		// Only an administrator edits, and only while the statements are still
		// under review — after that they are on their way to a database, or
		// they are the record of what ran.
		"CanEdit": user != nil && user.Role == auth.Admin &&
			detail.MigrationID != 0 &&
			detail.State != "READY_TO_EXECUTE" && detail.State != "EXECUTING" &&
			detail.State != "COMPLETED" && detail.State != "CLOSED" &&
			detail.State != "NEEDS_ATTENTION",
		"CanDecide": user != nil && user.Role.CanWrite() &&
			(detail.Author != user.Email ||
				(detail.Approval != nil && detail.Approval.SoleAdmin)),
		"IsAuthor": user != nil && detail.Author == user.Email,
	})
}

// act handles every write on a request: deciding, commenting, resolving a
// thread, regenerating.
//
// One handler because they share the same guards — writable scope, CSRF, and
// ownership of the request — and splitting them would mean repeating those
// three checks four times, which is how one of them ends up missing.
func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a request id", http.StatusBadRequest)
		return
	}
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())
	back := "/requests/" + strconv.FormatInt(id, 10)

	var actErr error
	switch r.FormValue("do") {
	case "decide":
		actErr = scope.Decide(r.Context(), id, user.ID,
			r.FormValue("verdict"), strings.TrimSpace(r.FormValue("comment")))
	case "comment":
		body := strings.TrimSpace(r.FormValue("body"))
		if threadID, err := strconv.ParseInt(r.FormValue("thread"), 10, 64); err == nil {
			actErr = scope.Reply(r.Context(), threadID, user.ID, body)
		} else {
			_, actErr = scope.StartThread(r.Context(), id, user.ID,
				strings.TrimSpace(r.FormValue("anchor")), body)
		}
	case "resolve":
		threadID, perr := strconv.ParseInt(r.FormValue("thread"), 10, 64)
		if perr != nil {
			actErr = errors.New("no thread named")
			break
		}
		actErr = scope.ResolveThread(r.Context(), threadID, user.ID,
			r.FormValue("resolution"))
	case "regenerate":
		_, actErr = scope.GenerateMigration(r.Context(), user.ID, id)
	case "execute":
		// The gate is re-evaluated inside EnqueueExecution rather than trusted
		// from when this page was rendered: an approval can be withdrawn and the
		// migration regenerated between someone seeing the button and pressing
		// it.
		actErr = scope.EnqueueExecution(r.Context(), user.ID, id)
	default:
		actErr = errors.New("unknown action")
	}

	if actErr != nil {
		// Shown on the page rather than as a bare error: every one of these is
		// something the reader can act on — approve as someone else, answer a
		// rename, resolve a thread.
		http.Redirect(w, r, back+"?error="+urlEscape(actErr.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "+", "&", "%26", "#", "%23", "?", "%3F").Replace(s)
}

var _ = store.RequestSummary{}

// activity renders the whole project's log, newest first.
//
// Filtered by level rather than by entity, because the question this page
// answers is "what is going on" and the useful narrowing is "what needs me" —
// which entity it concerns is what the request and database pages are for.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	level := r.URL.Query().Get("level")
	if level != "warn" && level != "error" {
		level = ""
	}
	entries, err := scope.ActivityFeed(r.Context(), 200, level)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "activity", "Activity", "activity", map[string]any{
		"Entries": entries,
		"Level":   level,
	})
}

// editStatement lets an administrator correct one statement of a migration or
// its revert.
//
// The generator gets things wrong — a cast with no USING clause, a change it
// can only render as a comment — and D-001 has always said a human must be able
// to correct the generated plan. Editing withdraws every approval and sends the
// migration back to be proven, so the correction is cheap to make and impossible
// to make quietly.
func (s *Server) editStatement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a request id", http.StatusBadRequest)
		return
	}
	if !checkCSRF(r) {
		http.Error(w, "invalid form token; reload the page and try again",
			http.StatusForbidden)
		return
	}
	scope, serr := s.scoped(r)
	if serr != nil {
		http.Error(w, serr.Error(), http.StatusInternalServerError)
		return
	}
	migrationID, err := strconv.ParseInt(r.FormValue("migration"), 10, 64)
	if err != nil {
		http.Error(w, "not a migration id", http.StatusBadRequest)
		return
	}
	ordinal, err := strconv.Atoi(r.FormValue("ordinal"))
	if err != nil {
		http.Error(w, "not a statement number", http.StatusBadRequest)
		return
	}

	back := "/requests/" + strconv.FormatInt(id, 10)
	err = scope.EditStatement(r.Context(), userFrom(r.Context()).ID, migrationID,
		r.FormValue("which") == "revert", ordinal, r.FormValue("sql"))
	if err != nil {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
