// Package drift decides whether a database has diverged from what it should be.
//
// Detection is fingerprint inequality and nothing more, which is why it works
// today: both sides of the comparison are already computable — the observed side
// by introspection, the expected side either from a declared schema or from
// another live database.
//
// Explaining a divergence — saying *what* changed — needs the semantic diff
// engine and does not exist yet. Detecting it does not, and detection is the
// half that wakes someone up.
package drift

import (
	"fmt"

	"github.com/rishabhju65/schemaver/internal/schema"
)

// Source is where an expectation came from.
type Source string

const (
	// FromDeclared compares against a repository's declared schema.
	FromDeclared Source = "declared"
	// FromPeer compares against another live database.
	FromPeer Source = "peer"
)

// Expectation is what a database is supposed to look like.
type Expectation struct {
	Source      Source
	Fingerprint schema.Version
	// PeerDatabaseID is set when Source is FromPeer.
	PeerDatabaseID int64
	// Label describes the expectation in the terms a human would use, so an
	// alert can say what it compared against rather than only that it compared.
	Label string
}

// Result is the outcome of one comparison.
type Result struct {
	// Comparable reports whether a comparison was possible at all. False means
	// there was nothing to compare against — not that the database is fine.
	Comparable bool
	Drifted    bool
	Observed   schema.Version
	Expected   schema.Version
	Source     Source
	Reason     string
}

// Evaluate compares an observed schema against its expectation.
//
// A missing expectation, or an expectation that has itself never been observed,
// yields Comparable false rather than "no drift". Reporting "no drift" for a
// database nothing is watching would be the most dangerous possible answer: it
// looks identical to a clean result.
func Evaluate(observed schema.Version, exp *Expectation) Result {
	if observed == "" {
		return Result{Reason: "database has not been read yet"}
	}
	if exp == nil {
		return Result{
			Observed: observed,
			Reason:   "no expectation configured: no repository linked and no peer set",
		}
	}
	if exp.Fingerprint == "" {
		return Result{
			Observed: observed,
			Source:   exp.Source,
			Reason: fmt.Sprintf("expectation %s has no known schema yet",
				exp.Label),
		}
	}

	r := Result{
		Comparable: true,
		Observed:   observed,
		Expected:   exp.Fingerprint,
		Source:     exp.Source,
		Drifted:    observed != exp.Fingerprint,
	}
	if r.Drifted {
		r.Reason = fmt.Sprintf("schema is %s but %s is %s",
			observed.Short(), exp.Label, exp.Fingerprint.Short())
	} else {
		r.Reason = fmt.Sprintf("schema matches %s", exp.Label)
	}
	return r
}
