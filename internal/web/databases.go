package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/rishabhju65/schemaver/internal/store"
)

// databaseNew adds one database, which is the thing somebody actually wants to
// manage. The server it lives on is recorded because databases sharing a host
// share its connection limit, not because a server is interesting by itself.
func (s *Server) databaseNew(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())

	servers, err := scope.Instances(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	environments, err := scope.Environments(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Most guarded first, so the browser's own default — the first option — is
	// the safe one. Environments are listed by ascending rank everywhere else,
	// which would have made staging the default here and quietly inverted the
	// argument the default exists to make.
	for i, j := 0, len(environments)-1; i < j; i, j = i+1, j-1 {
		environments[i], environments[j] = environments[j], environments[i]
	}

	form := map[string]string{
		"server": r.FormValue("server"), "name": r.FormValue("name"),
		"host": r.FormValue("host"), "port": r.FormValue("port"),
		"username": r.FormValue("username"), "database": r.FormValue("database"),
		"tls_mode": r.FormValue("tls_mode"), "environment": r.FormValue("environment"),
	}
	if form["port"] == "" {
		form["port"] = "5432"
	}
	render := func(cause error) {
		s.render(w, r, "database_new", "Add a database", "instances", map[string]any{
			"Servers": servers, "Environments": environments, "Form": form,
			"Error":    cause,
			"TLSModes": []string{"require", "verify-full", "disable"},
		})
	}
	if r.Method != http.MethodPost {
		render(nil)
		return
	}

	database := strings.TrimSpace(r.FormValue("database"))
	if database == "" {
		render(errors.New("which database on that server?"))
		return
	}
	// Asked rather than assumed, because somebody is standing here. Zero falls
	// back to the most guarded environment the project has, which is what
	// discovery used to do on nobody's behalf.
	environmentID, _ := strconv.ParseInt(r.FormValue("environment"), 10, 64)

	// An existing server is chosen by id, so its credentials are reused rather
	// than retyped — which is the whole ergonomic argument for keeping a server
	// record at all once the database is the unit.
	if existing := r.FormValue("server"); existing != "" && existing != "new" {
		instanceID, perr := strconv.ParseInt(existing, 10, 64)
		if perr != nil {
			render(errors.New("which server?"))
			return
		}
		if _, err := scope.AdoptDatabases(r.Context(), user.ID, instanceID,
			environmentID, []string{database}); err != nil {
			render(err)
			return
		}
		http.Redirect(w, r, "/instances/"+existing, http.StatusSeeOther)
		return
	}

	port, perr := strconv.Atoi(r.FormValue("port"))
	if perr != nil || port <= 0 {
		render(errors.New("a port is a number"))
		return
	}
	id, err := scope.AddDatabase(r.Context(), user.ID,
		r.FormValue("name"), strings.TrimSpace(r.FormValue("host")), port,
		r.FormValue("tls_mode"), strings.TrimSpace(r.FormValue("username")),
		r.FormValue("password"), database, environmentID)
	if err != nil {
		render(err)
		return
	}
	// Onboarding ends by asking what to watch, while somebody is still looking
	// at the database they just added — rather than leaving it as a setting to
	// discover after a schema they do not care about has been reporting drift
	// for a week.
	http.Redirect(w, r, "/database/"+strconv.FormatInt(id, 10)+"/watch?new=1",
		http.StatusSeeOther)
}

// adopt is the picker: what else is on this server, and which of it do you
// want. Looking commits you to nothing.
func (s *Server) adopt(w http.ResponseWriter, r *http.Request) {
	scope, err := s.scoped(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	instanceID, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		http.Error(w, "not a server id", http.StatusBadRequest)
		return
	}
	user := userFrom(r.Context())
	here := "/instances/" + strconv.FormatInt(instanceID, 10)

	if r.Method == http.MethodPost {
		chosen := r.Form["adopt"]
		if len(chosen) == 0 {
			http.Redirect(w, r, here, http.StatusSeeOther)
			return
		}
		// No per-database choice in the bulk path, so the safe default stands.
		if _, err := scope.AdoptDatabases(r.Context(), user.ID, instanceID, 0, chosen); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, here, http.StatusSeeOther)
		return
	}

	var candidates []store.Candidate
	var cause error
	candidates, cause = scope.Discoverable(r.Context(), instanceID)
	s.render(w, r, "adopt", "What else is here", "instances", map[string]any{
		"InstanceID": instanceID, "Candidates": candidates, "Error": cause,
	})
}
