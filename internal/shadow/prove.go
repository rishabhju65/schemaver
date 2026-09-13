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
	// RevertedTo is where the revert left the schema, when one was proven. A
	// revert that works returns it to Base.
	RevertedTo schema.Version
	// FinalSchema is the schema the statements produced, kept because the
	// caller deriving a migration from written SQL needs to diff against it and
	// rebuilding the shadow a second time to read it again would double the
	// cost of every derivation.
	FinalSchema *schema.Schema
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
func (p *Pool) Prove(ctx context.Context, baseDDL string, statements, revert []string, from, want schema.Version) (*Proof, error) {
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
		sch, err := introspect.Schema(ctx, conn)
		if err != nil {
			return proof, &StepError{Ordinal: i + 1, SQL: sql, Err: err}
		}
		after, err := schema.Fingerprint(sch)
		if err != nil {
			return proof, &StepError{Ordinal: i + 1, SQL: sql, Err: err}
		}
		proof.After = append(proof.After, after)
		proof.FinalSchema = sch
	}

	if len(proof.After) > 0 {
		proof.Final = proof.After[len(proof.After)-1]
	} else {
		proof.Final = proof.Base
	}
	if proof.Final != want {
		return proof, &MismatchError{Want: want, Got: proof.Final}
	}
	if len(revert) == 0 {
		return proof, nil
	}

	// The round trip. The forward migration has just been applied to this
	// database, so it is sitting exactly where a real one would be when
	// somebody asks to undo it — which makes this the only place the revert can
	// be checked against the state it is actually for.
	for i, sql := range revert {
		if err := exec(ctx, conn, sql); err != nil {
			return proof, &RevertError{Ordinal: i + 1, SQL: sql, Err: err}
		}
	}
	if proof.RevertedTo, err = fingerprint(ctx, conn); err != nil {
		return proof, &RevertError{Err: err}
	}
	if proof.RevertedTo != from {
		return proof, &RevertError{Want: from, Got: proof.RevertedTo}
	}
	return proof, nil
}

// RevertError reports that the way back does not lead back.
//
// Held apart from every forward failure because it says nothing about the
// migration: the forward statements have already been applied and verified by
// the time this can happen. A migration whose revert will not round-trip is
// still a correct migration, and describing it as a failed one would send
// somebody to change the half that works.
type RevertError struct {
	Ordinal   int
	SQL       string
	Want, Got schema.Version
	Err       error
}

func (e *RevertError) Error() string {
	switch {
	case e.Ordinal > 0:
		sql := strings.TrimSpace(e.SQL)
		if len(sql) > 120 {
			sql = sql[:117] + "..."
		}
		return fmt.Sprintf("revert statement %d would not apply (%s): %v",
			e.Ordinal, sql, e.Err)
	case e.Err != nil:
		return fmt.Sprintf("the schema could not be read back after reverting: %v", e.Err)
	default:
		return fmt.Sprintf(
			"every revert statement applied, but the schema came back to %s rather "+
				"than %s — undoing this migration would not return the database to "+
				"where it started", e.Got.Short(), e.Want.Short())
	}
}

func (e *RevertError) Unwrap() error { return e.Err }

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
