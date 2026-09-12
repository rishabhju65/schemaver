package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rishabhju65/schemaver/internal/store"
)

// retire shows what standing a database down would close, then does it.
//
// A confirmation page rather than a button in the settings form. Retiring
// cancels queued work and closes open change requests, and an action with
// consequences that outlive the click should say what they are first — the same
// reason the review page shows a diff before offering to run it.
func (s *Server) retire(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a database id", http.StatusBadRequest)
		return
	}
	scope, serr := s.scoped(r)
	if serr != nil {
		http.Error(w, serr.Error(), http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())

	preview, err := scope.PreviewRetirement(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodPost {
		if !checkCSRF(r) {
			http.Error(w, "invalid form token; reload the page and try again",
				http.StatusForbidden)
			return
		}
		// Typing the name is the confirmation. A database is retired once and
		// the request it closes cannot be reopened, so the cost of a misclick
		// is higher than the cost of typing eight characters.
		if strings.TrimSpace(r.FormValue("confirm")) != preview.DatabaseName {
			s.render(w, r, "retire", "Retire "+preview.DatabaseName, "fleet",
				map[string]any{
					"Preview": preview, "DatabaseID": id,
					"Error": "Type the database's name exactly to confirm.",
				})
			return
		}
		if _, err := scope.RetireDatabase(r.Context(), user.ID, id,
			strings.TrimSpace(r.FormValue("reason"))); err != nil {
			s.render(w, r, "retire", "Retire "+preview.DatabaseName, "fleet",
				map[string]any{"Preview": preview, "DatabaseID": id, "Error": err.Error()})
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.render(w, r, "retire", "Retire "+preview.DatabaseName, "fleet",
		map[string]any{"Preview": preview, "DatabaseID": id})
}

// restore brings a retired database back. No confirmation: it grants nothing
// that was not there before, and the schema is re-read before anything can be
// proposed against it.
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a database id", http.StatusBadRequest)
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
	if err := scope.RestoreDatabase(r.Context(), userFrom(r.Context()).ID, id); err != nil {
		http.Redirect(w, r, "/?error="+store.ErrNotWritable.Error(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
