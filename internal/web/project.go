package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/store"
)

// projectCookie remembers which project a user is looking at.
const projectCookie = "schemaver_project"

// allProjects is the value a project cookie takes when an organisation viewer is
// looking across every project at once.
const allProjects = "all"

type projectCtxKey int

const (
	membershipsKey projectCtxKey = iota
	currentKey
)

// withProjects attaches a user's memberships and their current project to the
// request, so handlers and templates need not each resolve it.
//
// The project named by the cookie is checked against membership on every
// request. A project id in a cookie is a claim by the caller, and treating it as
// anything else would make isolation a matter of what someone typed.
func (s *Server) withProjects(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r.Context())
		if user == nil {
			next.ServeHTTP(w, r)
			return
		}

		memberships, err := s.store.Memberships(r.Context(), user.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), membershipsKey, memberships)

		current := ""
		if c, err := r.Cookie(projectCookie); err == nil {
			current = c.Value
		}
		ctx = context.WithValue(ctx, currentKey, s.resolveCurrent(user, memberships, current))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveCurrent decides which project a request is about, ignoring any claim
// the caller is not entitled to.
func (s *Server) resolveCurrent(user *auth.User, memberships []store.Membership, claimed string) string {
	if claimed == allProjects && user.OrgRole.ReadsEverything() {
		return allProjects
	}
	if id, err := strconv.ParseInt(claimed, 10, 64); err == nil {
		for _, m := range memberships {
			if m.ProjectID == id {
				return claimed
			}
		}
		// Claimed a project they do not belong to: fall through to a default
		// rather than error, since a stale cookie is ordinary.
	}
	if len(memberships) > 0 {
		return strconv.FormatInt(memberships[0].ProjectID, 10)
	}
	if user.OrgRole.ReadsEverything() {
		return allProjects
	}
	return ""
}

func membershipsFrom(ctx context.Context) []store.Membership {
	m, _ := ctx.Value(membershipsKey).([]store.Membership)
	return m
}

func currentProject(ctx context.Context) string {
	c, _ := ctx.Value(currentKey).(string)
	return c
}

// scoped returns a store restricted to what this request may see.
//
// Every handler that reads or writes project data goes through this. The
// unscoped store is unreachable from a handler by design: isolation is a
// property of which type a method lives on, not of remembering to filter.
func (s *Server) scoped(r *http.Request) (*store.Scope, error) {
	user := userFrom(r.Context())
	if user == nil {
		// Unreachable behind requireUser. Binding to an impossible project means
		// a routing mistake returns nothing rather than everything.
		return s.store.ForProject(-1), nil
	}

	current := currentProject(r.Context())
	if current == allProjects && user.OrgRole.ReadsEverything() {
		return s.store.ForOrganization(r.Context(), user.OrganizationID)
	}
	if id, err := strconv.ParseInt(current, 10, 64); err == nil {
		return s.store.ForProject(id), nil
	}
	return s.store.ForProject(-1), nil
}

// switchProject records which project the caller wants to look at.
func (s *Server) switchProject(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	want := r.FormValue("project")
	// Validated the same way every request is, so a forged value selects
	// nothing rather than something.
	value := s.resolveCurrent(user, membershipsFrom(r.Context()), want)

	http.SetCookie(w, &http.Cookie{
		Name: projectCookie, Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
		MaxAge: int(auth.SessionLifetime.Seconds()),
	})
	back := r.Header.Get("Referer")
	if back == "" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
