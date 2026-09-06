// Package auth handles who may use schemaver and what they may do.
//
// The shape is deliberately small. This is a self-hosted tool inside a private
// network (D-005), not a consumer product: local accounts, server-side
// sessions, three roles. Anything more elaborate is surface area that has to be
// got right for no benefit schemaver actually needs.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Role is what an account may do.
//
// The split that matters is Viewer versus everything else: a Viewer can read
// every schema and divergence but cannot store a credential or register an
// instance. That is what makes a public demo safe to expose.
type Role string

const (
	// Admin manages accounts and instances.
	Admin Role = "admin"
	// Operator registers instances and manages databases, but not accounts.
	Operator Role = "operator"
	// Viewer reads only.
	Viewer Role = "viewer"
)

// Valid reports whether r is a role the system recognises.
func (r Role) Valid() bool { return r == Admin || r == Operator || r == Viewer }

// CanWrite reports whether the role may change anything.
func (r Role) CanWrite() bool { return r == Admin || r == Operator }

// CanManageUsers reports whether the role may create or modify accounts.
func (r Role) CanManageUsers() bool { return r == Admin }

// User is an account.
type User struct {
	ID          int64
	Email       string
	DisplayName string
	Role        Role
	Disabled    bool
}

// SessionLifetime is how long a session stays valid without use.
const SessionLifetime = 12 * time.Hour

// ErrBadCredentials is returned for any failed sign-in.
//
// One error for every cause, deliberately: distinguishing "no such account" from
// "wrong password" tells an attacker which addresses are registered.
var ErrBadCredentials = errors.New("email or password is incorrect")

// HashPassword returns a bcrypt hash suitable for storage.
func HashPassword(plain string) (string, error) {
	if err := CheckPasswordStrength(plain); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// VerifyPassword reports whether plain matches a stored hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// MinPasswordLength is the shortest password accepted.
//
// Length is the only rule. Composition requirements ("one symbol, one digit")
// measurably push people towards predictable substitutions without improving
// resistance to guessing, and this is a tool for a handful of engineers rather
// than a public sign-up.
const MinPasswordLength = 12

// CheckPasswordStrength rejects passwords too short to be worth hashing.
func CheckPasswordStrength(plain string) error {
	if len([]rune(strings.TrimSpace(plain))) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

// NewToken returns a URL-safe random token with 256 bits of entropy, used for
// both session cookies and the one-time setup token.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns the digest stored in place of a token.
//
// The token itself is never persisted, so a leaked database yields no usable
// session. A plain digest is right here where it would be wrong for a password:
// a 256-bit random token cannot be brute-forced, so the slow hashing that
// protects guessable secrets buys nothing and would cost a lookup on every
// request.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
