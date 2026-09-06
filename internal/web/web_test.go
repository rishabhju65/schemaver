package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rishabhju65/schemaver/internal/auth"
)

func server(t *testing.T, setup *auth.Setup, demo bool) *Server {
	t.Helper()
	s, err := New(nil, setup, demo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestTemplatesParse catches a broken template at build time rather than on a
// request. Every page shares one layout, so a syntax error in any of them takes
// down the whole interface.
func TestTemplatesParse(t *testing.T) {
	s := server(t, auth.Completed(), false)
	for _, page := range []string{"fleet", "history", "change", "drift", "login", "setup"} {
		if s.tmpl[page] == nil {
			t.Errorf("%s template missing", page)
		}
	}
}

// TestPagesRequireAnAccount is the guarantee that makes deploying this
// defensible. Every page carrying schema information must refuse an
// unauthenticated visitor — the README promised there was no auth, and this is
// what changes that.
func TestPagesRequireAnAccount(t *testing.T) {
	h := server(t, auth.Completed(), false).Handler()

	for _, path := range []string{"/", "/history", "/drift", "/database/1", "/change/1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s: got %d, want a redirect to sign-in", path, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s: redirected to %q, want /login", path, loc)
		}
	}
}

// TestSetupDivertsVisitorsButNotSignIn checks a deployment with no administrator
// sends visitors to setup — while leaving sign-in reachable.
//
// Accounts can exist before an administrator does: demo mode seeds a read-only
// one. Diverting /login too would make such an account impossible to use, which
// is precisely the deployment demo mode exists for.
func TestSetupDivertsVisitorsButNotSignIn(t *testing.T) {
	pending, err := auth.NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}
	h := server(t, pending, false).Handler()

	for _, path := range []string{"/", "/history", "/drift", "/instances"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if loc := rec.Header().Get("Location"); loc != "/setup" {
			t.Errorf("%s: redirected to %q, want /setup", path, loc)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/login: got %d, want 200 — an existing account must still be able to sign in", rec.Code)
	}
	if !containsAll(rec.Body.String(), "/setup") {
		t.Error("/login does not offer a route to setup while one is pending")
	}
}

// TestHealthzIsPublic keeps the readiness probe reachable without credentials;
// an orchestrator cannot sign in.
func TestHealthzIsPublic(t *testing.T) {
	rec := httptest.NewRecorder()
	server(t, auth.Completed(), false).Handler().
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
}

// TestLoginPageAdvertisesDemoOnlyInDemoMode guards against a build accidentally
// publishing a password.
func TestLoginPageAdvertisesDemoOnlyInDemoMode(t *testing.T) {
	for _, tc := range []struct {
		demo        bool
		wantVisible bool
	}{{false, false}, {true, true}} {
		rec := httptest.NewRecorder()
		server(t, auth.Completed(), tc.demo).Handler().
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

		shown := rec.Body.Len() > 0 &&
			containsAll(rec.Body.String(), DemoEmail, DemoPassword)
		if shown != tc.wantVisible {
			t.Errorf("demo=%v: credentials visible=%v, want %v", tc.demo, shown, tc.wantVisible)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestCSRFTokenIsDerivedAndUnguessable checks the form token cannot be produced
// without the session token it comes from.
func TestCSRFTokenIsDerivedAndUnguessable(t *testing.T) {
	a, _ := auth.NewToken()
	b, _ := auth.NewToken()

	if csrfToken(a) != csrfToken(a) {
		t.Error("csrfToken is not deterministic")
	}
	if csrfToken(a) == csrfToken(b) {
		t.Error("different sessions produced the same form token")
	}
	if csrfToken(a) == a {
		t.Error("the form token is the session token; it must not be exposed in HTML")
	}
}

// TestCSRFRejectsMissingAndWrongTokens covers the two ways a forged post arrives.
func TestCSRFRejectsMissingAndWrongTokens(t *testing.T) {
	sessionToken, _ := auth.NewToken()

	withCookie := func(form string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/x?"+form, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
		return req
	}

	if checkCSRF(httptest.NewRequest(http.MethodPost, "/x", nil)) {
		t.Error("a request with no session cookie passed the check")
	}
	if checkCSRF(withCookie("csrf=")) {
		t.Error("an empty form token passed")
	}
	if checkCSRF(withCookie("csrf=wrong")) {
		t.Error("a wrong form token passed")
	}
	if !checkCSRF(withCookie("csrf=" + csrfToken(sessionToken))) {
		t.Error("the correct form token was rejected")
	}
}

// TestWriteRoutesRequireAnAccount covers the surface that stores credentials.
// Reaching it without an account must never be possible.
func TestWriteRoutesRequireAnAccount(t *testing.T) {
	h := server(t, auth.Completed(), false).Handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/instances"},
		{http.MethodGet, "/instances/new"},
		{http.MethodPost, "/instances/new"},
		{http.MethodGet, "/instances/1"},
		{http.MethodPost, "/instances/1"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s %s: redirected to %q, want /login", tc.method, tc.path, loc)
		}
	}
}

// TestConnectErrorsAreActionable checks a failed connection names what to change.
// A generic "connection failed" costs a support round-trip every time.
func TestConnectErrorsAreActionable(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"password authentication failed for user \"x\"", "rejected these credentials"},
		{"dial tcp: lookup db.example: no such host", "could not be resolved"},
		{"dial tcp 10.0.0.1:5432: connection refused", "refused the connection"},
		{"dial tcp 10.0.0.1:5432: i/o timeout", "firewall or security group"},
		{"server does not support SSL, but SSL was required", "TLS negotiation failed"},
		{`database "nope" does not exist`, "pick a database that exists"},
	} {
		got := describeConnectError("db.example", errString(tc.raw)).Error()
		if !containsAll(got, tc.want) {
			t.Errorf("%q\n  got:  %s\n  want it to mention: %s", tc.raw, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestFormBuildsConnectionURL(t *testing.T) {
	f := connectionForm{
		Host: "db.internal", Port: "5433", Username: "schemaver",
		Password: "p@ss word/1", Database: "postgres", TLSMode: "verify-full",
	}
	dsn, port, err := f.dsn()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if port != 5433 {
		t.Errorf("port: got %d, want 5433", port)
	}
	for _, want := range []string{"db.internal:5433", "sslmode=verify-full", "connect_timeout=10"} {
		if !containsAll(dsn, want) {
			t.Errorf("dsn missing %q: %s", want, dsn)
		}
	}
	// A password with reserved characters must survive as credentials rather
	// than corrupting the URL.
	if containsAll(dsn, "p@ss word/1") {
		t.Errorf("password was not escaped: %s", dsn)
	}
}

func TestFormRejectsBadPort(t *testing.T) {
	for _, p := range []string{"0", "70000", "abc", "-1"} {
		f := connectionForm{Host: "h", Port: p, Username: "u", Database: "d", TLSMode: "require"}
		if _, _, err := f.dsn(); err == nil {
			t.Errorf("port %q was accepted", p)
		}
	}
}
