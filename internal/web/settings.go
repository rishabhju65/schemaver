package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/store"
)

// settings shows and changes what this project asks for before a change runs.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())

	var cause error
	if r.Method == http.MethodPost {
		approvals, perr := strconv.Atoi(r.FormValue("approvals"))
		if perr != nil {
			cause = errors.New("how many approvals? give a number")
		} else {
			cause = scope.SetPolicy(r.Context(), user.ID, store.Policy{
				ApprovalsRequired: approvals,
			})
		}
		if cause == nil {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
	}

	policy, err := scope.Policy(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "settings", "Settings", "settings", map[string]any{
		"Policy":  policy,
		"IsAdmin": user != nil && user.Role == auth.Admin,
		"Error":   cause,
		// Offered as a list rather than a free number: the useful values are
		// none, one and two, and a box accepting 4 invites somebody to discover
		// by trying that it will not accept 40.
		"Choices": []int{0, 1, 2, 3},
	})
}
