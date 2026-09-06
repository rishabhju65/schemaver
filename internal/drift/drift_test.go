package drift

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/schema"
)

const (
	live     = schema.Version("aaaa000000000000000000000000000000000000000000000000000000000000")
	declared = schema.Version("bbbb000000000000000000000000000000000000000000000000000000000000")
)

func declaredExp(fp schema.Version) *Expectation {
	return &Expectation{Source: FromDeclared, Fingerprint: fp, Label: "declared schema in payments at main"}
}

func peerExp(fp schema.Version) *Expectation {
	return &Expectation{Source: FromPeer, Fingerprint: fp, PeerDatabaseID: 7, Label: "orders on prod-1"}
}

// TestNotComparableIsNotClean is the distinction that matters most. A database
// with nothing to compare against and a database that matches its expectation
// both produce Drifted false — and mean opposite things. Anything that treats
// them alike reports "all clear" for databases nobody is watching.
func TestNotComparableIsNotClean(t *testing.T) {
	cases := map[string]Result{
		"never read":          Evaluate("", declaredExp(declared)),
		"no expectation set":  Evaluate(live, nil),
		"expectation unknown": Evaluate(live, declaredExp("")),
		"peer never observed": Evaluate(live, peerExp("")),
	}
	for name, got := range cases {
		t.Run(name, func(t *testing.T) {
			if got.Comparable {
				t.Error("reported as comparable when there was nothing to compare against")
			}
			if got.Drifted {
				t.Error("reported drift without a comparison")
			}
			if got.Reason == "" {
				t.Error("no reason given for why no comparison happened")
			}
		})
	}

	clean := Evaluate(live, declaredExp(live))
	if !clean.Comparable || clean.Drifted {
		t.Fatalf("a matching schema should be comparable and undrifted: %+v", clean)
	}
}

func TestDetectsDivergence(t *testing.T) {
	got := Evaluate(live, declaredExp(declared))
	if !got.Comparable || !got.Drifted {
		t.Fatalf("divergence not detected: %+v", got)
	}
	if got.Observed != live || got.Expected != declared {
		t.Errorf("wrong fingerprints recorded: observed %s expected %s", got.Observed, got.Expected)
	}
	if got.Source != FromDeclared {
		t.Errorf("source: got %s, want %s", got.Source, FromDeclared)
	}
}

func TestDetectsAgreement(t *testing.T) {
	got := Evaluate(live, peerExp(live))
	if !got.Comparable {
		t.Fatal("comparison should have been possible")
	}
	if got.Drifted {
		t.Errorf("identical fingerprints reported as drift: %s", got.Reason)
	}
	if !strings.Contains(got.Reason, "orders on prod-1") {
		t.Errorf("reason does not name what was compared against: %s", got.Reason)
	}
}

// TestReasonNamesBothSides checks an alert says what it compared against, not
// merely that something differs.
func TestReasonNamesBothSides(t *testing.T) {
	got := Evaluate(live, peerExp(declared))
	for _, want := range []string{live.Short(), declared.Short(), "orders on prod-1"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason omits %q\n  got: %s", want, got.Reason)
		}
	}
}

// TestPeerAndDeclaredBehaveIdentically confirms the pre-repository mode is a
// first-class path rather than a degraded one: comparing against a peer detects
// exactly what comparing against a declared schema does.
func TestPeerAndDeclaredBehaveIdentically(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected schema.Version
		drifted  bool
	}{
		{"matching", live, false},
		{"diverged", declared, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(live, declaredExp(tc.expected))
			p := Evaluate(live, peerExp(tc.expected))
			if d.Comparable != p.Comparable || d.Drifted != p.Drifted {
				t.Errorf("declared and peer disagree: declared=%+v peer=%+v", d, p)
			}
			if d.Drifted != tc.drifted {
				t.Errorf("drifted: got %v, want %v", d.Drifted, tc.drifted)
			}
		})
	}
}
