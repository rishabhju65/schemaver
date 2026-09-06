package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/auth"
)

// CountAdmins reports how many administrator accounts exist.
//
// Setup is gated on administrators rather than on accounts in general: demo mode
// seeds a read-only account, and counting that as "set up" would lock out
// creation of the first real administrator.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM schemaver.app_user
		  WHERE role = 'admin' AND disabled_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count administrators: %w", err)
	}
	return n, nil
}

// EnsureUser creates an account only if its email is not already taken, and
// reports whether it made one.
func (s *Store) EnsureUser(ctx context.Context, email, displayName string, role auth.Role, passwordHash string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schemaver.app_user WHERE email = lower($1))`,
		email).Scan(&exists); err != nil {
		return false, fmt.Errorf("check for user: %w", err)
	}
	if exists {
		return false, nil
	}
	if _, err := s.CreateUser(ctx, email, displayName, role, passwordHash); err != nil {
		return false, err
	}
	return true, nil
}

// CreateUser adds an account. The password is hashed before it reaches here.
func (s *Store) CreateUser(ctx context.Context, email, displayName string, role auth.Role, passwordHash string) (*auth.User, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	u := &auth.User{Email: email, DisplayName: displayName, Role: role}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO schemaver.app_user (email, display_name, password_hash, role)
		VALUES (lower($1), $2, $3, $4)
		RETURNING id`, email, displayName, passwordHash, string(role)).Scan(&u.ID)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// Authenticate verifies an email and password.
//
// A disabled account fails identically to a wrong password: whether an address
// is registered, or was, is not something an unauthenticated caller should be
// able to determine.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*auth.User, error) {
	var u auth.User
	var hash string
	var disabled *time.Time
	var role string

	err := s.pool.QueryRow(ctx, `
		SELECT id, email, COALESCE(display_name, ''), password_hash, role, disabled_at
		  FROM schemaver.app_user WHERE email = lower($1)`, email).
		Scan(&u.ID, &u.Email, &u.DisplayName, &hash, &role, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		// Hash anyway, so a missing account and a wrong password take
		// indistinguishable time.
		auth.VerifyPassword("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinva", password)
		return nil, auth.ErrBadCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}
	if !auth.VerifyPassword(hash, password) || disabled != nil {
		return nil, auth.ErrBadCredentials
	}
	u.Role = auth.Role(role)
	return &u, nil
}

// StartSession issues a session and returns the token to hand the browser. Only
// the token's digest is stored.
func (s *Store) StartSession(ctx context.Context, userID int64) (string, time.Time, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(auth.SessionLifetime)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO schemaver.session (token_hash, user_id, expires_at, last_used_at)
		VALUES ($1, $2, $3, now())`,
		auth.HashToken(token), userID, expires); err != nil {
		return "", time.Time{}, fmt.Errorf("start session: %w", err)
	}
	return token, expires, nil
}

// UserBySession resolves a session token to its account, refreshing last use.
//
// Expired sessions are deleted on the way past rather than merely rejected, so
// the table does not need a separate reaper for the common case.
func (s *Store) UserBySession(ctx context.Context, token string) (*auth.User, error) {
	if token == "" {
		return nil, auth.ErrBadCredentials
	}
	hash := auth.HashToken(token)

	var u auth.User
	var role string
	var disabled *time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE schemaver.session SET last_used_at = now()
		 WHERE token_hash = $1 AND expires_at > now()
		RETURNING (SELECT id FROM schemaver.app_user WHERE id = user_id),
		          (SELECT email FROM schemaver.app_user WHERE id = user_id),
		          (SELECT COALESCE(display_name, '') FROM schemaver.app_user WHERE id = user_id),
		          (SELECT role FROM schemaver.app_user WHERE id = user_id),
		          (SELECT disabled_at FROM schemaver.app_user WHERE id = user_id)`,
		hash).Scan(&u.ID, &u.Email, &u.DisplayName, &role, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = s.pool.Exec(ctx, `DELETE FROM schemaver.session WHERE token_hash = $1`, hash)
		return nil, auth.ErrBadCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}
	if disabled != nil {
		return nil, auth.ErrBadCredentials
	}
	u.Role = auth.Role(role)
	return &u, nil
}

// EndSession revokes one session.
func (s *Store) EndSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM schemaver.session WHERE token_hash = $1`, auth.HashToken(token)); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}

// PurgeExpiredSessions removes sessions nobody will present again.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM schemaver.session WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
