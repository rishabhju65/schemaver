package web

import (
	"net/http"
	"strconv"
)

// object shows what has happened to one table, enum or sequence on one
// database — the history of a single thing rather than of the schema it sits
// in.
func (s *Server) object(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	databaseID, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		http.Error(w, "not a database id", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	db, err := scope.Database(r.Context(), databaseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	events, err := scope.ObjectHistory(r.Context(), databaseID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "object", name, "history", map[string]any{
		"DB": db, "Name": name, "Events": events,
	})
}

// watch chooses which schemas inside a database are watched.
//
// Reached at the end of onboarding rather than offered as a setting to find
// later. A database arrives watching everything, which is right for the common
// shape — one schema called public — and wrong the moment a database holds
// somebody else's work as well. Asking once, while somebody is already looking
// at the database they just added, costs a click and saves them noticing later.
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	databaseID, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		http.Error(w, "not a database id", http.StatusBadRequest)
		return
	}
	user := userFrom(r.Context())

	if r.Method == http.MethodPost {
		// The form posts what to watch; what is stored is what to exclude, so
		// that a schema created tomorrow is watched without anybody revisiting
		// this.
		keep := map[string]bool{}
		for _, n := range r.Form["watch"] {
			keep[n] = true
		}
		all, err := scope.Namespaces(r.Context(), databaseID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		excluded := []string{}
		for _, ns := range all {
			if !keep[ns.Name] {
				excluded = append(excluded, ns.Name)
			}
		}
		if err := scope.WatchNamespaces(r.Context(), user.ID, databaseID, excluded); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/database/"+strconv.FormatInt(databaseID, 10), http.StatusSeeOther)
		return
	}

	db, err := scope.Database(r.Context(), databaseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	namespaces, err := scope.Namespaces(r.Context(), databaseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "watch", db.Name, "fleet", map[string]any{
		"DB": db, "Namespaces": namespaces,
		// Set when this is the last step of adding a database rather than a
		// revisit, so the page can say which it is.
		"Onboarding": r.URL.Query().Get("new") == "1",
	})
}
