package auth

import (
	"crypto/subtle"
	"errors"
	"sync"
)

// Setup guards creation of the first account on a fresh deployment.
//
// The token exists only in memory. It is never written down, so there is nothing
// to leak and nothing to revoke; restarting the process issues a new one and
// invalidates the old. Once an account exists the token is irrelevant, so the
// window in which it means anything is as small as it can be.
//
// The alternative — letting the first visitor claim the account — is a race:
// whoever reaches the port first owns the deployment, and on anything reachable
// that is not necessarily you.
type Setup struct {
	mu    sync.Mutex
	token string
	done  bool
}

// ErrSetupComplete is returned when an account already exists.
var ErrSetupComplete = errors.New("setup has already been completed")

// ErrBadSetupToken is returned for a token that does not match.
var ErrBadSetupToken = errors.New("setup token is incorrect")

// NewSetup issues a token for a deployment that has no accounts yet.
func NewSetup() (*Setup, error) {
	t, err := NewToken()
	if err != nil {
		return nil, err
	}
	return &Setup{token: t}, nil
}

// Completed returns a Setup for a deployment that already has accounts.
func Completed() *Setup { return &Setup{done: true} }

// Pending reports whether the first account still has to be created.
func (s *Setup) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.done
}

// Token returns the one-time token, or empty once setup is finished.
func (s *Setup) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return ""
	}
	return s.token
}

// Consume validates a token and marks setup complete.
//
// Comparison is constant-time so that a wrong answer reveals nothing about how
// wrong it was.
func (s *Setup) Consume(candidate string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return ErrSetupComplete
	}
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) != 1 {
		return ErrBadSetupToken
	}
	s.done = true
	s.token = ""
	return nil
}
