package store

import (
	"context"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/auth"
)

// Scope is a Store bound to one account.
//
// Isolation is enforced by construction rather than by discipline. Every query
// an account can reach lives on this type and carries its account id; the
// unscoped Store exposes only what is genuinely account-independent —
// authentication, migrations, and the worker's own job processing. Forgetting a
// WHERE clause is therefore not a mistake that can be made quietly: the method
// would have to be written on the wrong type first.
//
// That matters because the failure it prevents is silent. A missing filter does
// not error; it returns somebody else's databases.
type Scope struct {
	store   *Store
	account int64
}

// For binds a store to an account.
func (s *Store) For(accountID int64) *Scope {
	return &Scope{store: s, account: accountID}
}

// Account reports which account this scope is bound to.
func (s *Scope) Account() int64 { return s.account }

// ---------------------------------------------------------------- accounts

// Account is a tenant: the owner of instances, credentials and environments.
type Account struct {
	ID   int64
	Name string
}

// CreateAccount makes an account with its first administrator, and gives it a
// set of environments to start from.
//
// One transaction: an account without a user is unreachable, and a user without
// an account can see nothing, so a half-finished sign-up is worse than a failed
// one.
func (s *Store) CreateAccount(ctx context.Context, accountName, email, displayName, passwordHash string) (*Account, *auth.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	account := &Account{Name: accountName}
	if err := tx.QueryRow(ctx,
		`INSERT INTO schemaver.account (name) VALUES ($1) RETURNING id`,
		accountName).Scan(&account.ID); err != nil {
		return nil, nil, fmt.Errorf("create account: %w", err)
	}

	user := &auth.User{Email: email, DisplayName: displayName, Role: auth.Admin}
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.app_user (email, display_name, password_hash, role, account_id)
		VALUES (lower($1), $2, $3, 'admin', $4)
		RETURNING id`,
		email, displayName, passwordHash, account.ID).Scan(&user.ID); err != nil {
		return nil, nil, fmt.Errorf("create administrator: %w", err)
	}

	// Environments are per-account; a new account starting with none would have
	// nowhere to place a database.
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.environment (name, rank, account_id)
		VALUES ('development', 10, $1), ('staging', 20, $1), ('production', 30, $1)`,
		account.ID); err != nil {
		return nil, nil, fmt.Errorf("seed environments: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit: %w", err)
	}
	return account, user, nil
}

// EmailTaken reports whether an address is already registered.
//
// Deliberately available before sign-up rather than surfacing a constraint
// violation: a unique-index error message is not something to show a person.
func (s *Store) EmailTaken(ctx context.Context, email string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schemaver.app_user WHERE email = lower($1))`,
		email).Scan(&exists); err != nil {
		return false, fmt.Errorf("check email: %w", err)
	}
	return exists, nil
}

// AccountOf returns the account a user belongs to.
func (s *Store) AccountOf(ctx context.Context, userID int64) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx,
		`SELECT account_id FROM schemaver.app_user WHERE id = $1`, userID).Scan(&id); err != nil {
		return 0, fmt.Errorf("find account for user %d: %w", userID, err)
	}
	return id, nil
}
