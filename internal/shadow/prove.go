package shadow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rishabhju65/schemaver/internal/introspect"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// Proof is what a shadow run establishes about a migration.
type Proof struct {
	// Base is what the rendered starting schema actually fingerprinted as. It
	// should equal the migration's declared start; when it does not, the fault
	// is in schemaver's own DDL rendering rather than in the migration, and the
	// two failures deserve different words.
	Base schema.Version
	// After is the fingerprint the database reached after each statement, in
	// order. This is what turns "it failed somewhere in the middle" into "it
	// applied through statement four", which is the difference between a
	// recoverable incident and an investigation.
	After []schema.Version
	Final schema.Version
}

// StepError names the statement that would not apply.
//
// A migration that fails its proof fails at a particular statement, and saying
// which one is most of the value: the author reads one line rather than
// fourteen.
type StepError struct {
	Ordinal int
	SQL     string
	Err     error
}

func (e *StepError) Error() string {
	sql := strings.TrimSpace(e.SQL)
	if len(sql) > 120 {
		sql = sql[:117] + "..."
	}
	return fmt.Sprintf("statement %d would not apply (%s): %v", e.Ordinal, sql, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

// BaseError reports that the starting schema could not be rebuilt.
//
// Distinguished from every other failure because it means schemaver could not
// reproduce a schema it had already read — the proof is not merely failing, it
// is not being run. Treating this as "the migration is bad" would send somebody
// to rewrite a migration that was never the problem.
type BaseError struct {
	Want, Got schema.Version
	Err       error
}

func (e *BaseError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("the starting schema could not be rebuilt: %v", e.Err)
	}
	return fmt.Sprintf(
		"the starting schema rebuilt as %s rather than %s, so the shadow does not "+
			"begin where this migration begins; this is a defect in schemaver's DDL "+
			"rendering, not in the migration",
		e.Got.Short(), e.Want.Short())
}

func (e *BaseError) Unwrap() error { return e.Err }

// Prove applies a migration one statement at a time to a throwaway database
// built at `from`, and reports the fingerprint after each.
//
// One statement at a time rather than in a batch, because the per-step
// fingerprints are the point. Verify answers "does this migration produce the
// declared schema"; this answers that and "where would it stop", which is what
// the executor needs when a run stops halfway through a real database.
//
// This proves the schema outcome only. The shadow holds no data, so it cannot
// find rows that violate a new constraint, cannot estimate duration, and says
// nothing about lock behaviour under load. It must never be reported as
// evidence that a migration is safe (D-009's scope cut).
func (p *Pool) Prove(ctx context.Context, baseDDL string, statements []string, from, want schema.Version) (*Proof, error) {
	db, err := p.Create(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	conn, err := pgx.Connect(ctx, db.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to shadow database: %w", err)
	}
	defer conn.Close(context.Background())

	if err := exec(ctx, conn, baseDDL); err != nil {
		return nil, &BaseError{Want: from, Err: err}
	}
	proof := &Proof{}
	if proof.Base, err = fingerprint(ctx, conn); err != nil {
		return nil, &BaseError{Want: from, Err: err}
	}
	if proof.Base != from {
		return nil, &BaseError{Want: from, Got: proof.Base}
	}

	for i, sql := range statements {
		if err := exec(ctx, conn, sql); err != nil {
			return proof, &StepError{Ordinal: i + 1, SQL: sql, Err: err}
		}
		// Fingerprinted even for a statement that changes nothing — a comment
		// standing in for an unrenderable change, say. The chain has to have one
		// entry per statement or the executor cannot index into it by ordinal.
		after, err := fingerprint(ctx, conn)
		if err != nil {
			return proof, &StepError{Ordinal: i + 1, SQL: sql, Err: err}
		}
		proof.After = append(proof.After, after)
	}

	if len(proof.After) > 0 {
		proof.Final = proof.After[len(proof.After)-1]
	} else {
		proof.Final = proof.Base
	}
	if proof.Final != want {
		return proof, &MismatchError{Want: want, Got: proof.Final}
	}
	return proof, nil
}

// exec runs one statement, or nothing at all if it is only a comment.
//
// Statements go one at a time through the simple protocol, so each is its own
// implicit transaction. That is what lets a concurrent index build run here at
// all: it refuses to run inside a transaction block, which a multi-statement
// batch would be.
func exec(ctx context.Context, conn *pgx.Conn, sql string) error {
	if onlyComments(sql) {
		return nil
	}
	_, err := conn.Exec(ctx, sql)
	return err
}

// onlyComments reports SQL that would execute nothing — the placeholders
// rendered for changes schemaver cannot express. They are left in the chain so
// the ordinals line up, and the migration fails its proof at the end when the
// final fingerprint comes up short, which is the honest outcome.
func onlyComments(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

func fingerprint(ctx context.Context, conn *pgx.Conn) (schema.Version, error) {
	s, err := introspect.Schema(ctx, conn)
	if err != nil {
		return "", err
	}
	return schema.Fingerprint(s)
}
