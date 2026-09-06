package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/rishabhju65/schemaver/internal/auth"
)

// Scope is a Store restricted to what one caller may see.
//
// Isolation is enforced by construction rather than by discipline. Every query a
// project's data can be reached through lives on this type and carries the set of
// projects the caller may read; the unscoped Store exposes only what is genuinely
// ownership-independent — authentication, migrations, and the worker's own job
// processing. Forgetting a filter is therefore not a mistake that can be made
// quietly: the method would have to be written on the wrong type first.
//
// The set is a list rather than a single id so that one query shape serves both
// callers. A project member reads one project; an organisation viewer reads all
// of them. Neither can construct a query that reads none of them and therefore
// all of them.
type Scope struct {
	store *Store

	// projects is what may be read. Never empty for a usable scope.
	projects []int64

	// writable is the project that may be changed, or zero for a read-only
	// scope. An organisation viewer reads everything and owns nothing, and that
	// is expressed here rather than checked in each handler.
	writable int64
}

// ErrReadOnly is returned when a read-only scope attempts a change.
var ErrReadOnly = errors.New("this role can read across the organisation but cannot change anything")

// ForProject binds to one project, with permission to change it.
func (s *Store) ForProject(projectID int64) *Scope {
	return &Scope{store: s, projects: []int64{projectID}, writable: projectID}
}

// ForOrganization binds to every project in an organisation, read-only.
//
// The projects are resolved once, here, rather than joined in every query: it
// keeps the query shape identical to the single-project case, so there is one
// filter to get right instead of two.
func (s *Store) ForOrganization(ctx context.Context, orgID int64) (*Scope, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM schemaver.project WHERE organization_id = $1`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list projects in organisation %d: %w", orgID, err)
	}
	defer rows.Close()

	scope := &Scope{store: s}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		scope.projects = append(scope.projects, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(scope.projects) == 0 {
		// An organisation with no projects must still produce a scope that reads
		// nothing rather than one that reads everything.
		scope.projects = []int64{-1}
	}
	return scope, nil
}

// Visible returns the projects this scope may read, for use as a query argument.
func (s *Scope) Visible() []int64 { return s.projects }

// Project returns the project this scope may change, or zero if read-only.
func (s *Scope) Project() int64 { return s.writable }

// CanWrite reports whether this scope may change anything.
func (s *Scope) CanWrite() bool { return s.writable != 0 }

// requireWrite guards every mutating method.
func (s *Scope) requireWrite() error {
	if !s.CanWrite() {
		return ErrReadOnly
	}
	return nil
}

// ------------------------------------------------- organisations and projects

// Organization is the top of the hierarchy: a company, holding projects.
type Organization struct {
	ID   int64
	Name string
}

// Project owns instances, credentials and environments.
type Project struct {
	ID   int64
	Name string
}

// CreateOrganization makes an organisation with a first project and the user who
// administers it.
//
// One transaction: an organisation with no project owns nothing, a project with
// no member is unreachable, and a user in neither can see nothing — so a
// half-finished sign-up is worse than a failed one.
func (s *Store) CreateOrganization(ctx context.Context, orgName, projectName, email, displayName, passwordHash string) (*Organization, *Project, *auth.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	org := &Organization{Name: orgName}
	if err := tx.QueryRow(ctx,
		`INSERT INTO schemaver.organization (name) VALUES ($1) RETURNING id`,
		orgName).Scan(&org.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("create organisation: %w", err)
	}

	project := &Project{Name: projectName}
	if err := tx.QueryRow(ctx,
		`INSERT INTO schemaver.project (name, organization_id) VALUES ($1, $2) RETURNING id`,
		projectName, org.ID).Scan(&project.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("create project: %w", err)
	}

	user := &auth.User{Email: email, DisplayName: displayName, Role: auth.Admin}
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemaver.app_user
		    (email, display_name, password_hash, role, organization_id)
		VALUES (lower($1), $2, $3, 'admin', $4)
		RETURNING id`,
		email, displayName, passwordHash, org.ID).Scan(&user.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("create administrator: %w", err)
	}
	user.OrganizationID = org.ID

	if _, err := tx.Exec(ctx,
		`INSERT INTO schemaver.project_member (project_id, user_id, role)
		 VALUES ($1, $2, 'admin')`, project.ID, user.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("add administrator to project: %w", err)
	}

	// Environments belong to a project; one starting with none would have
	// nowhere to place a database.
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemaver.environment (name, rank, project_id)
		VALUES ('development', 10, $1), ('staging', 20, $1), ('production', 30, $1)`,
		project.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("seed environments: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit: %w", err)
	}
	return org, project, user, nil
}

// EmailTaken reports whether an address is already registered.
func (s *Store) EmailTaken(ctx context.Context, email string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schemaver.app_user WHERE email = lower($1))`,
		email).Scan(&exists); err != nil {
		return false, fmt.Errorf("check email: %w", err)
	}
	return exists, nil
}

// Membership is a project a user belongs to, and what they may do in it.
type Membership struct {
	ProjectID int64
	Name      string
	Role      auth.Role
}

// Memberships lists the projects a user belongs to, in name order.
func (s *Store) Memberships(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.project_id, p.name, m.role
		  FROM schemaver.project_member m
		  JOIN schemaver.project p ON p.id = m.project_id
		 WHERE m.user_id = $1
		 ORDER BY p.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		var role string
		if err := rows.Scan(&m.ProjectID, &m.Name, &role); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		m.Role = auth.Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberOf reports whether a user belongs to a project, and in what role.
//
// Consulted on every request that names a project, so that a project id in a URL
// is never trusted on its own.
func (s *Store) MemberOf(ctx context.Context, userID, projectID int64) (auth.Role, bool, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM schemaver.project_member WHERE user_id = $1 AND project_id = $2`,
		userID, projectID).Scan(&role)
	if err != nil {
		return "", false, nil
	}
	return auth.Role(role), true, nil
}
