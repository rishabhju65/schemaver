package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rishabhju65/schemaver/internal/store"
)

func (s *Server) branches(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := scope.Branches(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	candidates, err := scope.Candidates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "branches", "Branches", "branches", map[string]any{
		"Rows": rows, "Candidates": candidates,
	})
}

// branchNew cuts a branch from a database's current schema.
func (s *Server) branchNew(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())

	databaseID, perr := strconv.ParseInt(r.FormValue("database"), 10, 64)
	if perr != nil {
		s.branchError(w, r, errors.New("choose a database to branch from"))
		return
	}
	id, err := scope.CutBranch(r.Context(), user.ID, databaseID,
		r.FormValue("name"), r.FormValue("description"))
	if err != nil {
		s.branchError(w, r, err)
		return
	}
	http.Redirect(w, r, "/branches/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// branchError re-renders the list with the reason, rather than replacing the
// page with a bare error: the reader is mid-task and the form they were filling
// in is on it.
func (s *Server) branchError(w http.ResponseWriter, r *http.Request, cause error) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, _ := scope.Branches(r.Context())
	candidates, _ := scope.Candidates(r.Context())
	s.render(w, r, "branches", "Branches", "branches", map[string]any{
		"Rows": rows, "Candidates": candidates, "Error": cause,
	})
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		http.Error(w, "not a branch id", http.StatusBadRequest)
		return
	}

	b, err := scope.Branch(r.Context(), id)
	if errors.Is(err, store.ErrNoSuchBranch) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// What diverged, and the history that got there. Both are read even when
	// the branch has not moved: "nothing yet" is an answer worth showing, and
	// an empty section reads better than a missing one.
	diverged, err := scope.BranchDiff(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	commits, err := scope.BranchCommits(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	candidates, err := scope.Candidates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// What merging into the origin would do, worked out now rather than on the
	// click. A conflict is the thing somebody most needs to know before they
	// have composed a change request around it.
	var merge *store.BranchMerge
	if b.OriginID != 0 && b.Diverged() {
		if m, err := scope.PlanBranchMerge(r.Context(), id, b.OriginID); err == nil {
			merge = m
		}
	}

	// Requests already open from this branch, so somebody arriving at the page
	// sees them before pressing the button that would make another.
	live, err := scope.LiveRequests(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// A database this branch already has a request against is not somewhere it
	// can go, so it is not offered. Same rule Candidates already applies to
	// databases that cannot be diffed or written to: a choice that will be
	// refused at the next step should not be a choice.
	taken := make(map[int64]bool, len(live))
	for _, lr := range live {
		taken[lr.DatabaseID] = true
	}
	open := make([]store.DatabaseRow, 0, len(candidates))
	for _, c := range candidates {
		if !taken[c.ID] {
			open = append(open, c)
		}
	}

	data := map[string]any{
		"B": b, "Diverged": diverged, "Commits": commits,
		"Candidates": open, "Merge": merge, "LiveRequests": live,
		// Only while a write is genuinely in flight. A page that reloads when
		// nothing is happening throws away whatever the reader was doing.
		"Refresh": b.Applying,
	}
	if msg := r.URL.Query().Get("error"); msg != "" {
		data["Error"] = errors.New(msg)
	}
	s.render(w, r, "branch", b.Name, "branches", data)
}

// branchAct carries out what the branch page offers: writing statements,
// merging into a database, and closing.
func (s *Server) branchAct(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		http.Error(w, "not a branch id", http.StatusBadRequest)
		return
	}
	user := userFrom(r.Context())
	here := "/branches/" + strconv.FormatInt(id, 10)

	var actErr error
	switch r.FormValue("do") {
	case "write":
		actErr = scope.WriteToBranch(r.Context(), user.ID, id,
			r.FormValue("sql"), r.FormValue("message"))
	case "merge":
		databaseID, err := strconv.ParseInt(r.FormValue("database"), 10, 64)
		if err != nil {
			actErr = errors.New("choose a database to merge into")
			break
		}
		requestID, err := scope.MergeBranch(r.Context(), user.ID, id, databaseID,
			r.FormValue("title"), r.FormValue("description"))
		if err != nil {
			actErr = err
			break
		}
		http.Redirect(w, r, "/requests/"+strconv.FormatInt(requestID, 10), http.StatusSeeOther)
		return
	case "close":
		actErr = scope.CloseBranch(r.Context(), user.ID, id, r.FormValue("reason"))
	default:
		actErr = errors.New("no action named")
	}

	if actErr != nil {
		// Carried in the query string so the reason survives the redirect that
		// stops a refresh repeating the action.
		http.Redirect(w, r, here+"?error="+url.QueryEscape(actErr.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, here, http.StatusSeeOther)
}
