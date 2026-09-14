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
