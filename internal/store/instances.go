package store

import (
	"context"

	"fmt"
	"github.com/jackc/pgx/v5"
	"time"
)

// InstanceRow is one registered server, for the instances list.
type InstanceRow struct {
	ID        int64
	Name      string
	Host      string
	Port      int
	TLSMode   string
	Username  string
	Engine    string
	Databases int
	Managed   int
	LastSeen  *time.Time
}

// Instances lists registered servers.
func (s *Scope) Instances(ctx context.Context) ([]InstanceRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT i.id, i.name, i.host, i.port, i.tls_mode, c.username, i.engine,
		       (SELECT count(*) FROM schemaver.database d
		         WHERE d.instance_id = i.id AND d.archived_at IS NULL),
		       (SELECT count(*) FROM schemaver.database d
		         WHERE d.instance_id = i.id AND d.archived_at IS NULL AND d.managed),
		       (SELECT max(d.last_checked_at) FROM schemaver.database d
		         WHERE d.instance_id = i.id)
		  FROM schemaver.instance i
		  JOIN schemaver.credential c ON c.id = i.credential_id
		 WHERE i.archived_at IS NULL AND i.project_id = ANY($1)
		 ORDER BY i.name`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()

	var out []InstanceRow
	for rows.Next() {
		var r InstanceRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Host, &r.Port, &r.TLSMode,
			&r.Username, &r.Engine, &r.Databases, &r.Managed, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ManagedDatabase is one database on an instance, with how it is configured.
// Maintenance reports the database every PostgreSQL installation creates for
// itself.
//
// `initdb` makes one called postgres as a place to connect before any real
// database exists, and it is also schemaver's own default for the database to
// open first. It is almost never something to version-control, and it turns up
// on servers whose console does not admit to it — a Neon project shows one
// database in its dashboard and has two on the endpoint, because Neon creates
// this one and does not surface it. Somebody who has just registered a server
// then finds a database they are certain they did not create.
//
// A hint, not a restriction: it can still be managed by anyone who means to.
func (d ManagedDatabase) Maintenance() bool { return d.Name == "postgres" }

type ManagedDatabase struct {
	ID   int64
	Name string
	// Managed is the soft toggle: whether schemaver watches this database.
	// Retired is the end-state, and outranks it — a retired database is never
	// managed, and the settings form must not offer to turn it back on.
	Managed       bool
	Retired       *time.Time
	RetiredReason string
	EnvironmentID *int64
	PeerID        *int64
	SizeBytes     int64
	LastError     string
}

// InstanceDetail returns one instance and every database discovered on it.
func (s *Scope) InstanceDetail(ctx context.Context, id int64) (*InstanceRow, []ManagedDatabase, error) {
	var inst InstanceRow
	err := s.store.pool.QueryRow(ctx, `
		SELECT i.id, i.name, i.host, i.port, i.tls_mode, c.username, i.engine
		  FROM schemaver.instance i
		  JOIN schemaver.credential c ON c.id = i.credential_id
		 WHERE i.id = $1 AND i.archived_at IS NULL AND i.project_id = ANY($2)`, id, s.projects).
		Scan(&inst.ID, &inst.Name, &inst.Host, &inst.Port, &inst.TLSMode,
			&inst.Username, &inst.Engine)
	if err != nil {
		return nil, nil, fmt.Errorf("load instance %d: %w", id, err)
	}

	rows, err := s.store.pool.Query(ctx, `
		SELECT id, name, managed, retired_at, COALESCE(retired_reason, ''),
		       environment_id, expected_peer_id,
		       COALESCE(size_bytes, 0), COALESCE(last_error, '')
		  FROM schemaver.database
		 WHERE instance_id = $1 AND archived_at IS NULL
		   AND instance_id IN (SELECT id FROM schemaver.instance WHERE project_id = ANY($2))
		 ORDER BY name`, id, s.projects)
	if err != nil {
		return nil, nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	var dbs []ManagedDatabase
	for rows.Next() {
		var d ManagedDatabase
		if err := rows.Scan(&d.ID, &d.Name, &d.Managed, &d.Retired, &d.RetiredReason,
			&d.EnvironmentID,
			&d.PeerID, &d.SizeBytes, &d.LastError); err != nil {
			return nil, nil, fmt.Errorf("scan database: %w", err)
		}
		dbs = append(dbs, d)
	}
	return &inst, dbs, rows.Err()
}

// EnvironmentRow is a deployment tier.
type EnvironmentRow struct {
	ID   int64
	Name string
	Rank int
}

// Environments lists tiers in promotion order.
func (s *Scope) Environments(ctx context.Context) ([]EnvironmentRow, error) {
	rows, err := s.store.pool.Query(ctx,
		`SELECT id, name, rank FROM schemaver.environment
		  WHERE project_id = ANY($1) ORDER BY rank`, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	var out []EnvironmentRow
	for rows.Next() {
		var e EnvironmentRow
		if err := rows.Scan(&e.ID, &e.Name, &e.Rank); err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DatabaseSettings is one database's configuration as submitted by a form.
type DatabaseSettings struct {
	ID            int64
	Managed       bool
	EnvironmentID *int64
	PeerID        *int64
}

// ApplyDatabaseSettings saves the management choices for an instance.
//
// Applied in one transaction so a partly-saved form cannot leave half the
// databases observed and half not.
func (s *Scope) ApplyDatabaseSettings(ctx context.Context, instanceID int64, settings []DatabaseSettings) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Which databases had their predecessor changed. Both checks that follow the
	// loop need it, and it is only knowable before the update runs.
	var repointed []int64
	var unpaired []int64
	for _, set := range settings {
		// Before anything reads or writes on account of this id.
		//
		// The UPDATE further down is scoped, so the row itself was safe — but
		// everything around it took the id on trust: the read below, the drift
		// rows closed when a database is unpaired, and the read queued when one
		// is re-pointed. A form naming another tenant's database therefore
		// resolved their alarms and made schemaver connect to their server,
		// while the row edit correctly did nothing. That asymmetry is what made
		// it quiet.
		//
		// Scoped to the instance in the URL as well as to the project, so the
		// address of the page constrains what the page may act on. Refused with
		// the same words as a missing database, because a refusal that tells
		// "somebody else's" from "not there" is a way to count what others have.
		var ours bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM schemaver.database d
				  JOIN schemaver.instance i ON i.id = d.instance_id
				 WHERE d.id = $1 AND d.instance_id = $2
				   AND i.project_id = ANY($3))`,
			set.ID, instanceID, s.projects).Scan(&ours); err != nil {
			return fmt.Errorf("check the database: %w", err)
		}
		if !ours {
			return fmt.Errorf("no such database")
		}

		var was *int64
		if err := tx.QueryRow(ctx,
			`SELECT expected_peer_id FROM schemaver.database WHERE id = $1`,
			set.ID).Scan(&was); err != nil {
			return fmt.Errorf("read the current predecessor: %w", err)
		}
		switch {
		case was == nil && set.PeerID == nil:
			// Unchanged and unset.
		case was != nil && set.PeerID != nil && *was == *set.PeerID:
			// Unchanged.
		case set.PeerID == nil:
			unpaired = append(unpaired, set.ID)
		default:
			repointed = append(repointed, set.ID)
		}

		// A database compared against itself would report drift against its own
		// schema forever; the database rejects it, but catching it here gives a
		// better message than a constraint violation.
		if set.PeerID != nil && *set.PeerID == set.ID {
			return fmt.Errorf("a database cannot be compared against itself")
		}
		// The link now also says which environment a change passes through
		// before this one, so pointing it at a higher environment states that
		// production precedes staging. Refused rather than warned about: it
		// would make the execute gate require production to have a change
		// before staging could have it, which is the sequence this exists to
		// prevent.
		// The values written here are as attacker-controlled as the row they are
		// written to, and only the row was being checked. The UPDATE below
		// scopes which database may be changed; it said nothing about what it
		// may be changed *to*, so a crafted form could point one tenant's
		// database at another tenant's — leaking that database's name onto the
		// fleet, the drift page and the database page, and putting its
		// fingerprint into the attacker's drift records once the comparison ran.
		//
		// Scoping a write means scoping both ends of it.
		if set.PeerID != nil {
			var ours bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM schemaver.database d
					  JOIN schemaver.instance i ON i.id = d.instance_id
					 WHERE d.id = $1 AND i.project_id = ANY($2))`,
				*set.PeerID, s.projects).Scan(&ours); err != nil {
				return fmt.Errorf("check the database being followed: %w", err)
			}
			if !ours {
				return fmt.Errorf("no such database")
			}
		}
		if set.EnvironmentID != nil {
			var ours bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM schemaver.environment e
					 WHERE e.id = $1 AND e.project_id = ANY($2))`,
				*set.EnvironmentID, s.projects).Scan(&ours); err != nil {
				return fmt.Errorf("check the environment: %w", err)
			}
			if !ours {
				return fmt.Errorf("no such environment")
			}
		}

		// A chain that comes back to where it started cannot be satisfied: each
		// database waits for the one below it to reach a schema, and in a cycle
		// every member is below every other, so none may go first. It deadlocks
		// the gate silently — nothing errors, the requests simply never become
		// executable and the reason each gives names a database that is waiting
		// on it.
		//
		// The rank check below catches the common shape of this, but only when
		// both databases carry an environment. Plenty do not, and that is
		// allowed, so the cycle has to be refused on its own terms.
		if set.PeerID != nil {
			var cycles bool
			if err := tx.QueryRow(ctx, `
				WITH RECURSIVE chain(id) AS (
					SELECT $1::bigint
					UNION
					SELECT d.expected_peer_id FROM schemaver.database d
					  JOIN chain c ON c.id = d.id
					 WHERE d.expected_peer_id IS NOT NULL
				)
				SELECT EXISTS (SELECT 1 FROM chain WHERE id = $2)`,
				*set.PeerID, set.ID).Scan(&cycles); err != nil {
				return fmt.Errorf("check for a cycle: %w", err)
			}
			if cycles {
				return fmt.Errorf(
					"that would make a loop: the database you are pointing at already " +
						"follows this one, directly or through others, and a change " +
						"cannot reach either first")
			}
		}
		if set.PeerID != nil {
			ok, higher, lower, err := s.ordered(ctx, tx, *set.PeerID, set.ID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf(
					"%s is a higher environment than %s, so a change cannot reach "+
						"%s first; this link says which database a change passes "+
						"through before reaching here", higher, lower, higher)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.database
			   SET managed = $2, environment_id = $3, expected_peer_id = $4
			 WHERE id = $1 AND instance_id = $5
			   AND instance_id IN (SELECT id FROM schemaver.instance WHERE project_id = ANY($6))`,
			set.ID, set.Managed, set.EnvironmentID, set.PeerID, instanceID,
			s.projects); err != nil {
			return fmt.Errorf("save settings for database %d: %w", set.ID, err)
		}
	}

	// A database that now follows nothing cannot be drifting from anything. The
	// schemas still differ, but "drift" is divergence from an expectation, and
	// the expectation has just been withdrawn — leaving the row open would have
	// the page report a comparison against a database this one no longer
	// follows, which a reader cannot tell from a current one.
	//
	// It matters more than it reads: unpairing is also how a change is taken out
	// from behind the promotion gate (D-024), so it is a deliberate act whose
	// effects should be visible immediately rather than in five minutes.
	if len(unpaired) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE schemaver.drift
			   SET status = 'resolved', resolved_at = now()
			 WHERE database_id = ANY($1) AND status = 'open'`, unpaired); err != nil {
			return fmt.Errorf("close drift for databases that now follow nothing: %w", err)
		}
	}

	// One that follows something new is compared against it on the next
	// observation, which is up to an interval away — so the page would show a
	// divergence from the old predecessor until then. Asking for a read now
	// costs one job and makes the change take effect when it is made.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// Queued after the commit, so a read is never asked for on account of a
	// change that did not take.
	for _, id := range repointed {
		if err := s.store.ObserveNow(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ordered reports whether a promotion link runs from a lower environment to a
// higher one.
//
// Permissive where it cannot tell. A database with no environment set, or a
// pair sharing one, says nothing about sequence — plenty of deployments never
// label their databases, and refusing those would make the field compulsory by
// the back door.
func (s *Scope) ordered(ctx context.Context, tx pgx.Tx, sourceID, targetID int64) (ok bool, source, target string, err error) {
	var sourceRank, targetRank *int
	var sourceName, targetName string
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT e.rank FROM schemaver.environment e WHERE e.id = a.environment_id),
		       (SELECT e.rank FROM schemaver.environment e WHERE e.id = b.environment_id),
		       a.name, b.name
		  FROM schemaver.database a, schemaver.database b
		 WHERE a.id = $1 AND b.id = $2`, sourceID, targetID).
		Scan(&sourceRank, &targetRank, &sourceName, &targetName); err != nil {
		return false, "", "", fmt.Errorf("compare environments: %w", err)
	}
	if sourceRank == nil || targetRank == nil {
		return true, sourceName, targetName, nil
	}
	return *sourceRank <= *targetRank, sourceName, targetName, nil
}

// PeerChoice is a database that could be followed, and where it lives.
type PeerChoice struct {
	ID int64
	// Name is the database; Server is the host it is on. Both are shown,
	// because a project with a staging and a production server very often has
	// the same database name on each, and "neondb" twice is not a choice.
	Name   string
	Server string
	// Environment is carried so the list can be read at a glance: the one
	// somebody wants is almost always the one ranked below.
	Environment string
	// SameServer marks the ones that used to be the only options, so the list
	// can keep them together rather than interleaving by name.
	SameServer bool

	// Managed reports whether the database is actually read.
	//
	// An unmanaged one can be followed and the result is a dead end: nothing
	// observes it, so its schema never moves, drift against it is never
	// comparable and the gate waiting on it never opens. Offered anyway, with
	// this said — hiding it would turn "why is my database not in the list"
	// into a mystery, and the answer is worth more than the absence.
	Managed bool
}

// Label is how the choice reads in a list.
func (p PeerChoice) Label() string {
	label := p.Name + " · " + p.Server
	if p.Environment != "" {
		label += " (" + p.Environment + ")"
	}
	if !p.Managed {
		label += " — not managed, so nothing would ever be compared"
	}
	return label
}

// PeerChoices lists every database in the project that could be followed.
//
// Every database, not every sibling. A change reaches production through
// staging, and staging is normally on its own server precisely so that its load
// never touches production's — so offering only databases on the same host
// makes the promotion chain unusable for the arrangement it exists to serve.
// Nothing underneath ever required them to be neighbours: the peer is validated
// against the project, the cycle check walks the chain wherever it goes, and the
// direction check compares environments rather than addresses.
//
// The database itself is excluded, which matters more now the list spans
// servers: a name that appears on two hosts is easy to pick by mistake, and a
// database following itself is a cycle of one.
func (s *Scope) PeerChoices(ctx context.Context, exclude int64) ([]PeerChoice, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT d.id, d.name, i.name, COALESCE(e.name, ''),
		       d.instance_id = (SELECT instance_id FROM schemaver.database WHERE id = $1),
		       d.managed
		  FROM schemaver.database d
		  JOIN schemaver.instance i ON i.id = d.instance_id
		  LEFT JOIN schemaver.environment e ON e.id = d.environment_id
		 WHERE i.project_id = ANY($2)
		   AND d.id <> $1
		   AND d.archived_at IS NULL AND d.retired_at IS NULL
		   AND i.archived_at IS NULL
		 ORDER BY d.managed DESC,
		          (d.instance_id = (SELECT instance_id FROM schemaver.database WHERE id = $1)) DESC,
		          i.name, d.name`, exclude, s.projects)
	if err != nil {
		return nil, fmt.Errorf("list databases that could be followed: %w", err)
	}
	defer rows.Close()

	var out []PeerChoice
	for rows.Next() {
		var p PeerChoice
		if err := rows.Scan(&p.ID, &p.Name, &p.Server, &p.Environment,
			&p.SameServer, &p.Managed); err != nil {
			return nil, fmt.Errorf("scan peer choice: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
