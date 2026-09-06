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

// Demo account credentials, used only when demo mode is switched on.
//
// The account is a Viewer, so publishing its password gives a stranger the
// ability to read the demo and nothing else — it cannot register a database or
// store a credential. That is what makes advertising it defensible.
const (
	DemoEmail    = "demo@schemaver.local"
	DemoPassword = "demo-read-only-account"
)

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
		if err != nil {
			// The session is gone or expired; clear the stale cookie so the
			// browser stops presenting it.
			clearSession(w, r)
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

// requireUser admits any signed-in account.
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.setup.Pending() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if userFrom(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
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
		Secure:   r.TLS != nil,
		MaxAge:   int(auth.SessionLifetime.Seconds()),
	})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil, MaxAge: -1,
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.setup.Pending() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
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

// setupHandler creates the first account, gated by the one-time token.
func (s *Server) setupHandler(w http.ResponseWriter, r *http.Request) {
	if !s.setup.Pending() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		s.renderAuth(w, r, "setup", "Set up", nil)
		return
	}

	fail := func(err error) { s.renderAuth(w, r, "setup", "Set up", err) }

	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		fail(err)
		return
	}
	// The token is consumed only once everything else has been accepted, so a
	// rejected password does not burn it and strand the operator.
	if err := s.setup.Consume(strings.TrimSpace(r.FormValue("token"))); err != nil {
		fail(err)
		return
	}
	user, err := s.store.CreateUser(r.Context(),
		strings.TrimSpace(r.FormValue("email")),
		strings.TrimSpace(r.FormValue("display_name")), auth.Admin, hash)
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
