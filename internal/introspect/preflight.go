package introspect

import (
	"context"
	"fmt"
)

// DatabaseAccess reports whether the connecting role can reach one database.
type DatabaseAccess struct {
	Name       string `json:"name"`
	CanConnect bool   `json:"can_connect"`
}

// Preflight describes what the connecting role can actually do, so onboarding
// can say what will and will not work before anything is stored.
//
// The point it exists to make: reading a schema needs almost nothing. pg_catalog
// is readable by every role, so introspection works with CONNECT alone — the
// account cannot read a single row of anyone's data. Stating that concretely, on
// the screen where someone is deciding whether to hand over a credential, is
// worth more than any amount of documentation.
type Preflight struct {
	ServerVersion string           `json:"server_version"`
	User          string           `json:"user"`
	Superuser     bool             `json:"superuser"`
	CanCreateDB   bool             `json:"can_create_db"`
	Databases     []DatabaseAccess `json:"databases"`
}

// Reachable counts the databases this role can connect to.
func (p *Preflight) Reachable() int {
	n := 0
	for _, d := range p.Databases {
		if d.CanConnect {
			n++
		}
	}
	return n
}

// Unreachable lists the databases the role cannot connect to.
func (p *Preflight) Unreachable() []string {
	var out []string
	for _, d := range p.Databases {
		if !d.CanConnect {
			out = append(out, d.Name)
		}
	}
	return out
}

// MissingGrants returns the SQL that would give this role access to every
// database it currently cannot reach.
//
// Returning the exact statement rather than describing the problem is the
// difference between a screen someone can act on and one they have to research.
func (p *Preflight) MissingGrants() []string {
	var out []string
	for _, name := range p.Unreachable() {
		out = append(out, fmt.Sprintf(
			`GRANT CONNECT ON DATABASE %q TO %q;`, name, p.User))
	}
	return out
}

// Check inspects what the connecting role may do.
func Check(ctx context.Context, q Querier) (*Preflight, error) {
	var p Preflight
	rows, err := q.Query(ctx, `
		SELECT current_setting('server_version'),
		       current_user,
		       COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false),
		       COALESCE((SELECT rolcreatedb FROM pg_roles WHERE rolname = current_user), false)`)
	if err != nil {
		return nil, fmt.Errorf("read server identity: %w", err)
	}
	if rows.Next() {
		if err := rows.Scan(&p.ServerVersion, &p.User, &p.Superuser, &p.CanCreateDB); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan server identity: %w", err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	access, err := q.Query(ctx, `
		SELECT d.datname, has_database_privilege(d.datname, 'CONNECT')
		  FROM pg_database d
		 WHERE NOT d.datistemplate AND d.datallowconn
		 ORDER BY d.datname`)
	if err != nil {
		return nil, fmt.Errorf("check database access: %w", err)
	}
	defer access.Close()

	for access.Next() {
		var d DatabaseAccess
		if err := access.Scan(&d.Name, &d.CanConnect); err != nil {
			return nil, fmt.Errorf("scan database access: %w", err)
		}
		p.Databases = append(p.Databases, d)
	}
	return &p, access.Err()
}
