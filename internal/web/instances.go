package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/store"
)

// tlsModes are the sslmode values PostgreSQL accepts, ordered from least to most
// protective.
var tlsModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// connectionForm is what the add-server page collects.
type connectionForm struct {
	Name, Host, Port, Username, Password, Database, TLSMode string
}

func formFrom(r *http.Request) connectionForm {
	f := connectionForm{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Host:     strings.TrimSpace(r.FormValue("host")),
		Port:     strings.TrimSpace(r.FormValue("port")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Password: r.FormValue("password"),
		Database: strings.TrimSpace(r.FormValue("database")),
		TLSMode:  r.FormValue("tls_mode"),
	}
	if f.Port == "" {
		f.Port = "5432"
	}
	if f.Database == "" {
		f.Database = "postgres"
	}
	if f.TLSMode == "" {
		f.TLSMode = "require"
	}
	return f
}

// dsn renders the form as a connection URL.
func (f connectionForm) dsn() (string, int, error) {
	port, err := strconv.Atoi(f.Port)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port must be a number between 1 and 65535")
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(f.Username, f.Password),
		Host:   fmt.Sprintf("%s:%d", f.Host, port),
		Path:   "/" + f.Database,
	}
	q := url.Values{}
	q.Set("sslmode", f.TLSMode)
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u.String(), port, nil
}

// describeConnectError turns a driver error into something an operator can act
// on. A single "connection failed" costs a support round-trip every time.
func describeConnectError(host string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "password authentication failed"),
		strings.Contains(msg, "role") && strings.Contains(msg, "does not exist"):
		return fmt.Errorf("the server rejected these credentials: %s", msg)
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "no route to host"):
		return fmt.Errorf("%s could not be resolved — check the hostname", host)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("%s refused the connection — check the port, and that this deployment can reach it", host)
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "context deadline"):
		return fmt.Errorf("%s did not answer — usually a firewall or security group between here and there", host)
	case strings.Contains(msg, "SSL"), strings.Contains(msg, "TLS"), strings.Contains(msg, "tls"):
		return fmt.Errorf("TLS negotiation failed: %s — try a different TLS mode", msg)
	case strings.Contains(msg, "does not exist"):
		return fmt.Errorf("%s — pick a database that exists; it is used only to enumerate the server", msg)
	default:
		return err
	}
}

func (s *Server) instances(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Instances(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "instances", "Servers", "instances", map[string]any{"Rows": rows})
}

// instanceNew tests a connection and, once it has succeeded, registers it.
//
// Testing is a separate submission on purpose: a credential is stored only after
// the operator has seen proof that it works and what it can reach.
func (s *Server) instanceNew(w http.ResponseWriter, r *http.Request) {
	form := connectionForm{Port: "5432", Database: "postgres", TLSMode: "require"}
	data := func(f connectionForm, pre *introspect.Preflight, cause error) map[string]any {
		m := map[string]any{"Form": f, "TLSModes": tlsModes, "Preflight": pre}
		if cause != nil {
			m["Error"] = cause.Error()
		}
		return m
	}

	if r.Method == http.MethodGet {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, nil, nil))
		return
	}

	form = formFrom(r)
	dsn, port, err := form.dsn()
	if err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, nil, err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances",
			data(form, nil, describeConnectError(form.Host, err)))
		return
	}
	defer conn.Close(context.Background())

	pre, err := introspect.Check(ctx, conn)
	if err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, nil, err))
		return
	}
	if r.FormValue("action") != "register" {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, pre, nil))
		return
	}

	name := form.Name
	if name == "" {
		name = fmt.Sprintf("%s:%d", form.Host, port)
	}
	id, err := s.store.RegisterInstance(ctx, name, form.Host, port,
		form.TLSMode, form.Username, form.Password)
	if err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, pre, err))
		return
	}
	found, err := introspect.Databases(ctx, conn)
	if err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, pre, err))
		return
	}
	// Discovered, not managed: choosing what to observe is the next screen.
	if _, _, err := s.store.SyncDatabases(ctx, id, found); err != nil {
		s.render(w, r, "instance_new", "Add a server", "instances", data(form, pre, err))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/instances/%d", id), http.StatusSeeOther)
}

func (s *Server) instanceDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not a server id", http.StatusBadRequest)
		return
	}

	var saveErr error
	saved := false
	if r.Method == http.MethodPost {
		if u := userFrom(r.Context()); u == nil || !u.Role.CanWrite() {
			http.Error(w, "this account is read-only", http.StatusForbidden)
			return
		}
		if !checkCSRF(r) {
			http.Error(w, "invalid form token; reload the page and try again",
				http.StatusForbidden)
			return
		}
		saveErr = s.saveInstanceSettings(r, id)
		saved = saveErr == nil
	}

	inst, dbs, err := s.store.InstanceDetail(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	envs, err := s.store.Environments(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m := map[string]any{
		"Instance": inst, "Databases": dbs, "Environments": envs, "Saved": saved,
	}
	if saveErr != nil {
		m["Error"] = saveErr.Error()
	}
	s.render(w, r, "instance", inst.Name, "instances", m)
}

// saveInstanceSettings applies the management form.
func (s *Server) saveInstanceSettings(r *http.Request, instanceID int64) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	// Checkboxes only submit when ticked, so an unticked database is absent
	// rather than false — the full list comes from the hidden "db" fields.
	managed := map[int64]bool{}
	for _, v := range r.Form["managed"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			managed[id] = true
		}
	}

	var settings []store.DatabaseSettings
	for _, v := range r.Form["db"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		settings = append(settings, store.DatabaseSettings{
			ID:            id,
			Managed:       managed[id],
			EnvironmentID: optionalID(r.FormValue(fmt.Sprintf("env-%d", id))),
			PeerID:        optionalID(r.FormValue(fmt.Sprintf("peer-%d", id))),
		})
	}
	return s.store.ApplyDatabaseSettings(r.Context(), instanceID, settings)
}

// optionalID parses a select value that may be the empty "no choice" option.
func optionalID(v string) *int64 {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}
