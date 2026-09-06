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

// TestSetupDivertsEverything checks a deployment with no administrator sends
// visitors to setup rather than to a sign-in page no account can pass.
func TestSetupDivertsEverything(t *testing.T) {
	pending, err := auth.NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}
	h := server(t, pending, false).Handler()

	for _, path := range []string{"/", "/history", "/login"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if loc := rec.Header().Get("Location"); loc != "/setup" {
			t.Errorf("%s: redirected to %q, want /setup", path, loc)
		}
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
