package auth

import (
	"errors"
	"strings"
	"testing"
)

// TestViewerCannotWrite pins the property a public demo depends on: a Viewer can
// read everything and change nothing, so exposing one cannot let a stranger
// store credentials or register an instance.
func TestViewerCannotWrite(t *testing.T) {
	if Viewer.CanWrite() {
		t.Error("Viewer may write; a demo account would be able to store credentials")
	}
	if Viewer.CanManageUsers() {
		t.Error("Viewer may manage users")
	}
	if !Operator.CanWrite() || !Admin.CanWrite() {
		t.Error("Operator and Admin must be able to write")
	}
	if Operator.CanManageUsers() {
		t.Error("Operator may manage users; only Admin should")
	}
	if !Admin.CanManageUsers() {
		t.Error("Admin cannot manage users")
	}
}

func TestRoleValidity(t *testing.T) {
	for _, r := range []Role{Admin, Operator, Viewer} {
		if !r.Valid() {
			t.Errorf("%s reported invalid", r)
		}
	}
	for _, r := range []Role{"", "root", "ADMIN", "superuser"} {
		if r.Valid() {
			t.Errorf("%q accepted as a role", r)
		}
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("the password is present in its own hash")
	}
	if !VerifyPassword(hash, pw) {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, pw+"x") {
		t.Error("incorrect password accepted")
	}
}

func TestPasswordsSaltIndependently(t *testing.T) {
	a, _ := HashPassword("correct horse battery staple")
	b, _ := HashPassword("correct horse battery staple")
	if a == b {
		t.Fatal("identical passwords produced identical hashes; they are not salted")
	}
}

func TestShortPasswordsRejected(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("a five-character password was accepted")
	}
	if err := CheckPasswordStrength(strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Errorf("a password of exactly the minimum length was rejected: %v", err)
	}
}

func TestTokensAreUniqueAndOpaque(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[tok] {
			t.Fatal("NewToken repeated a value")
		}
		seen[tok] = true
		if len(tok) < 40 {
			t.Fatalf("token is only %d characters; too little entropy", len(tok))
		}
	}
}

func TestHashTokenIsStableAndNotReversible(t *testing.T) {
	tok, _ := NewToken()
	if string(HashToken(tok)) != string(HashToken(tok)) {
		t.Error("HashToken is not deterministic")
	}
	if strings.Contains(string(HashToken(tok)), tok) {
		t.Error("the token appears inside its own digest")
	}
	other, _ := NewToken()
	if string(HashToken(tok)) == string(HashToken(other)) {
		t.Error("different tokens produced the same digest")
	}
}

// TestSetupTokenIsSingleUse covers the bootstrap guarantee: exactly one account
// can be created with a given token, and only by someone holding it.
func TestSetupTokenIsSingleUse(t *testing.T) {
	s, err := NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}
	if !s.Pending() {
		t.Fatal("a fresh deployment should be pending setup")
	}

	if err := s.Consume("wrong-token"); !errors.Is(err, ErrBadSetupToken) {
		t.Errorf("wrong token: got %v, want ErrBadSetupToken", err)
	}
	if !s.Pending() {
		t.Error("a failed attempt completed setup")
	}

	if err := s.Consume(s.Token()); err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
	if s.Pending() {
		t.Error("setup still pending after being consumed")
	}
	if s.Token() != "" {
		t.Error("the token is still readable after use")
	}
	if err := s.Consume("anything"); !errors.Is(err, ErrSetupComplete) {
		t.Errorf("second use: got %v, want ErrSetupComplete", err)
	}
}

func TestCompletedSetupAcceptsNothing(t *testing.T) {
	s := Completed()
	if s.Pending() {
		t.Error("a deployment with accounts reported as pending setup")
	}
	if err := s.Consume(""); !errors.Is(err, ErrSetupComplete) {
		t.Errorf("got %v, want ErrSetupComplete", err)
	}
}
