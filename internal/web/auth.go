package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/rishabhju65/schemaver/internal/auth"
)

// sessionCookie is the name of the cookie carrying the session token.
const sessionCookie = "schemaver_session"

type ctxKey int

const userKey ctxKey = iota

// userFrom returns the signed-in account, or nil.
func userFrom(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey).(*auth.User)
	return u
}

// csrfToken derives a form token from the session token.
//
// Double-submit, without a second cookie: the session token lives in an
// HttpOnly cookie a script cannot read, and this derivative is embedded in
// forms. A cross-site request can cause the cookie to be sent but cannot learn
// the derived value, so it cannot produce a form that validates.
func csrfToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("schemaver-csrf:" + sessionToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	want := csrfToken(c.Value)
	got := r.FormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// authenticate resolves the session cookie and attaches the account to the
// request context. It never rejects: enforcement is the guards' job, so that a
// signed-out visitor reaching a public path is not treated as an error.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, err := s.store.UserBySession(r.Context(), c.Value)
		if errors.Is(err, auth.ErrBadCredentials) {
			// Gone or expired; clear the stale cookie so the browser stops
			// presenting it.
			clearSession(w, r)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			// A database fault is not an expired session. Treating them alike
			// silently signs everyone out and hides the cause — which is exactly
			// what happened when a column rename left this query stale.
			http.Error(w, "could not resolve session: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

// requireUser admits any signed-in account.
//
// A signed-in account is admitted even while setup is pending. Accounts can
// exist before an administrator does — demo mode seeds a read-only one — and
// there is no reason to bar a valid session from the application merely because
// nobody has claimed the administrator role yet. Only a visitor with no session
// is sent to setup, and only when there is a setup to complete.
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) != nil {
			h(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// requireWriter admits accounts that may change something.
//
// The refusal is deliberately explicit rather than a redirect: a Viewer who
// reaches a write path has not lost their session, and bouncing them to a login
// page they are already past would be a confusing lie.
func (s *Server) requireWriter(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r.Context()); u == nil || !u.Role.CanWrite() {
			http.Error(w, "this account is read-only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !checkCSRF(r) {
			http.Error(w, "invalid form token; reload the page and try again",
				http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// isSecure reports whether the request reached us over TLS, including when a
// reverse proxy terminated it.
//
// Trusting X-Forwarded-Proto here is safe in the direction that matters. Marking
// a cookie Secure when the connection is actually plain HTTP only stops the
// browser sending it — an inconvenience. Failing to mark it when the connection
// *is* HTTPS is what leaks a session, so erring towards Secure is the correct
// bias, and a forged header only restricts the forger's own cookie.
func isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		// Lax rather than Strict: Strict would sign a user out whenever they
		// arrive from a link someone pasted in chat, which is how these pages
		// are actually shared.
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(r),
		MaxAge:   int(auth.SessionLifetime.Seconds()),
	})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: isSecure(r), MaxAge: -1,
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderAuth(w, r, "login", "Sign in", nil)
		return
	}

	user, err := s.store.Authenticate(r.Context(),
		strings.TrimSpace(r.FormValue("email")), r.FormValue("password"))
	if err != nil {
		if !errors.Is(err, auth.ErrBadCredentials) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.renderAuth(w, r, "login", "Sign in", err)
		return
	}
	token, _, err := s.store.StartSession(r.Context(), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.EndSession(r.Context(), c.Value)
	}
	clearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// signupHandler creates an account and its first administrator.
//
// Accounts are isolated from one another, so this is not privileged: creating
// one grants access to nothing that already exists. When open sign-up is off it
// is gated by the one-time setup token, which is how a closed deployment gets
// its first account without exposing registration to anyone who finds the URL.
func (s *Server) signupHandler(w http.ResponseWriter, r *http.Request) {
	needsToken := !s.openSignup && s.setup.Pending()
	if !s.openSignup && !s.setup.Pending() {
		// Closed, and already set up: there is no route to a new account.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	data := map[string]any{"NeedsToken": needsToken}
	if r.Method == http.MethodGet {
		s.renderAuthWith(w, r, "signup", "Create an account", nil, data)
		return
	}

	fail := func(err error) { s.renderAuthWith(w, r, "signup", "Create an account", err, data) }

	email := strings.TrimSpace(r.FormValue("email"))
	taken, err := s.store.EmailTaken(r.Context(), email)
	if err != nil {
		fail(err)
		return
	}
	if taken {
		fail(errors.New("that email address is already registered"))
		return
	}

	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		fail(err)
		return
	}
	// The token is consumed only once everything else has been accepted, so a
	// rejected password does not burn it and strand the operator.
	if needsToken {
		if err := s.setup.Consume(strings.TrimSpace(r.FormValue("token"))); err != nil {
			fail(err)
			return
		}
	}

	orgName := strings.TrimSpace(r.FormValue("organization"))
	if orgName == "" {
		orgName = email
	}
	projectName := strings.TrimSpace(r.FormValue("project"))
	if projectName == "" {
		projectName = "default"
	}
	_, _, user, err := s.store.CreateOrganization(r.Context(), orgName, projectName,
		email, strings.TrimSpace(r.FormValue("display_name")), hash)
	if err != nil {
		fail(err)
		return
	}

	token, _, err := s.store.StartSession(r.Context(), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
