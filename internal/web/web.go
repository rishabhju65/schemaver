// Package web serves schemaver's interface.
//
// It is rendered on the server. That is a starting point, not the end state:
// D-002 commits to a visual review surface with ER diagrams and an interactive
// semantic diff, and those need a real client application. Server rendering
// gets the history and fleet views usable now, with no build step and nothing to
// install, and none of it has to be thrown away — these pages stay useful as
// plain, linkable, fast-loading views.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/history"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

//go:embed templates/*.html
var files embed.FS

// Server renders the interface over a Store.
type Server struct {
	store *store.Store
	tmpl  map[string]*template.Template
	setup *auth.Setup
	// demo advertises a read-only account on the sign-in page. It is opt-in and
	// loudly indicated, because it publishes a password on purpose.
	demo bool
}

// funcs are the helpers templates use to render values a person can read.
var funcs = template.FuncMap{
	"short": func(v schema.Version) string { return v.Short() },
	"mb": func(b int64) string {
		if b <= 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	},
	// eq64 compares an optional selection against a candidate id, for marking
	// the chosen option in a select.
	"eq64":  func(a *int64, b int64) bool { return a != nil && *a == b },
	"stamp": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	// ago renders staleness rather than hiding it: a view that shows only "last
	// read" without its age presents stale data as current.
	"ago": func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		d := time.Since(*t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		default:
			return fmt.Sprintf("%dd ago", int(d.Hours()/24))
		}
	},
}

// New parses the templates and wires the routes.
//
// setup carries the one-time bootstrap token; pass auth.Completed() for a
// deployment that already has accounts.
func New(s *store.Store, setup *auth.Setup, demo bool) (*Server, error) {
	if setup == nil {
		setup = auth.Completed()
	}
	srv := &Server{store: s, tmpl: map[string]*template.Template{}, setup: setup, demo: demo}
	for _, page := range []string{"fleet", "history", "change", "drift", "login", "setup",
		"instances", "instance_new", "instance"} {
		t, err := template.New("layout").Funcs(funcs).ParseFS(files,
			"templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, err)
		}
		srv.tmpl[page] = t
	}
	return srv, nil
}

// Handler returns the router.
//
// Every page except sign-in, setup and the health check requires an account.
// There is no anonymous read path even in demo mode: a demo signs in as a
// Viewer, which keeps one authentication path rather than a second, less
// exercised one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.requireUser(s.fleet))
	mux.HandleFunc("GET /history", s.requireUser(s.history))
	mux.HandleFunc("GET /database/{id}", s.requireUser(s.database))
	mux.HandleFunc("GET /change/{id}", s.requireUser(s.change))
	mux.HandleFunc("GET /drift", s.requireUser(s.drift))
	mux.HandleFunc("GET /instances", s.requireUser(s.instances))
	mux.HandleFunc("GET /instances/{id}", s.requireUser(s.instanceDetail))

	// Registering a server stores a credential, so these need an account that
	// may write — a demo Viewer must never reach them.
	mux.HandleFunc("GET /instances/new", s.requireWriter(s.instanceNew))
	mux.HandleFunc("POST /instances/new", s.requireWriter(s.instanceNew))
	mux.HandleFunc("POST /instances/{id}", s.requireUser(s.instanceDetail))

	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /logout", s.logout)
	mux.HandleFunc("GET /setup", s.setupHandler)
	mux.HandleFunc("POST /setup", s.setupHandler)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return s.authenticate(mux)
}

// renderAuth draws the sign-in and setup pages, which have no account attached.
func (s *Server) renderAuth(w http.ResponseWriter, r *http.Request, page, title string, cause error) {
	data := map[string]any{
		"MinPassword":  auth.MinPasswordLength,
		"DemoEmail":    DemoEmail,
		"DemoPassword": DemoPassword,
	}
	if cause != nil {
		data["Error"] = cause.Error()
		w.WriteHeader(http.StatusUnauthorized)
	}
	s.renderWith(w, r, page, title, "", data)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title, nav string, data map[string]any) {
	s.renderWith(w, r, page, title, nav, data)
}

func (s *Server) renderWith(w http.ResponseWriter, r *http.Request, page, title, nav string, data map[string]any) {
	data["Title"], data["Nav"] = title, nav
	data["Demo"] = s.demo
	user := userFrom(r.Context())
	data["User"] = user
	data["CanWrite"] = user != nil && user.Role.CanWrite()
	if c, err := r.Cookie(sessionCookie); err == nil {
		data["CSRF"] = csrfToken(c.Value)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[page].ExecuteTemplate(w, "layout", data); err != nil {
		// The response is already partly written by this point, so the only
		// honest thing left is to log it; changing the status would be a lie.
		http.Error(w, "render failed: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) fleet(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Fleet(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "fleet", "Fleet", "fleet", map[string]any{"Rows": rows})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.Timeline(r.Context(), store.TimelineFilter{Limit: 200})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "history", "History", "history",
		map[string]any{"Entries": entries, "Database": ""})
}

func (s *Server) database(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a database id", http.StatusBadRequest)
		return
	}
	entries, err := s.store.Timeline(r.Context(),
		store.TimelineFilter{DatabaseID: id, Limit: 200})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("database %d", id)
	if len(entries) > 0 {
		name = entries[0].Database
	}
	s.render(w, r, "history", name, "history",
		map[string]any{"Entries": entries, "Database": name})
}

// change shows which objects differ across one recorded transition.
func (s *Server) change(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a snapshot id", http.StatusBadRequest)
		return
	}
	entries, err := s.store.Timeline(r.Context(), store.TimelineFilter{Limit: 500})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var entry *store.TimelineEntry
	for i := range entries {
		if entries[i].SnapshotID == id {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		http.Error(w, "change not found", http.StatusNotFound)
		return
	}

	before, err := s.store.Blob(r.Context(), entry.From)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	after, err := s.store.Blob(r.Context(), entry.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	changes := history.ObjectsChanged(before, after)
	s.render(w, r, "change", "Change", "history", map[string]any{
		"Entry":   entry,
		"Changes": changes,
		"Summary": history.Count(changes),
	})
}

func (s *Server) drift(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Drifts(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "drift", "Drift", "drift", map[string]any{"Rows": rows})
}
