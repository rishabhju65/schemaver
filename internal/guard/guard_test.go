package guard

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/plan"
	"github.com/rishabhju65/schemaver/internal/schema"
)

// A four-step migration: from → f1 → f2 → f3 → to.
const (
	from  = schema.Version("aaaa000000000000000000000000000000000000000000000000000000000000")
	f1    = schema.Version("1111000000000000000000000000000000000000000000000000000000000000")
	f2    = schema.Version("2222000000000000000000000000000000000000000000000000000000000000")
	f3    = schema.Version("3333000000000000000000000000000000000000000000000000000000000000")
	to    = schema.Version("bbbb000000000000000000000000000000000000000000000000000000000000")
	alien = schema.Version("dead000000000000000000000000000000000000000000000000000000000000")
)

func migration() *plan.Migration {
	return &plan.Migration{
		Name: "0042_add_status",
		From: from,
		To:   to,
		Steps: []plan.Step{
			{Ordinal: 1, ExpectedAfter: f1, Transactional: true},
			{Ordinal: 2, ExpectedAfter: f2, Transactional: false},
			{Ordinal: 3, ExpectedAfter: f3, Transactional: true},
			{Ordinal: 4, ExpectedAfter: to, Transactional: true},
		},
	}
}

// TestClassify walks every row of D-013's reconciliation table.
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		live     schema.Version
		recorded Recorded
		want     Verdict
		steps    int
		safe     bool
	}{
		{"completed and database agrees", to, RecordedCompleted, Proceed, 4, true},
		{"completed but never applied", from, RecordedCompleted, HaltDrifted, 0, false},
		{"completed but only partly applied", f2, RecordedCompleted, HaltPartial, 2, false},

		{"running, actually finished", to, RecordedRunning, SelfHeal, 4, true},
		{"running, nothing applied", from, RecordedRunning, NothingApplied, 0, true},
		{"running, stopped midway", f1, RecordedRunning, HaltPartial, 1, false},
		{"running, stopped at last step", f3, RecordedRunning, HaltPartial, 3, false},

		{"failed, actually finished", to, RecordedFailed, SelfHeal, 4, true},
		{"failed, rolled back cleanly", from, RecordedFailed, NothingApplied, 0, true},
		{"failed, left partial state", f2, RecordedFailed, HaltPartial, 2, false},

		{"schema matches nothing known", alien, RecordedRunning, HaltUnknown, 0, false},
		{"schema matches nothing, recorded complete", alien, RecordedCompleted, HaltUnknown, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.live, migration(), tc.recorded)
			if got.Verdict != tc.want {
				t.Errorf("verdict: got %s, want %s\n  reason: %s", got.Verdict, tc.want, got.Reason)
			}
			if got.StepsApplied != tc.steps {
				t.Errorf("steps applied: got %d, want %d", got.StepsApplied, tc.steps)
			}
			if got.Safe() != tc.safe {
				t.Errorf("safe: got %v, want %v", got.Safe(), tc.safe)
			}
			if got.Reason == "" {
				t.Error("no reason given; an operator needs to know why")
			}
		})
	}
}

// TestClassifyNoPrevious covers a database that has never been migrated.
func TestClassifyNoPrevious(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev *plan.Migration
		rec  Recorded
	}{
		{"nil previous migration", nil, RecordedCompleted},
		{"nothing recorded", migration(), RecordedNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(alien, tc.prev, tc.rec)
			if got.Verdict != Proceed || !got.Safe() {
				t.Errorf("got %s, want proceed", got.Verdict)
			}
		})
	}
}

// TestHaltReasonsAreActionable checks a halting verdict names the versions an
// operator needs. A halt that says only "inconsistent state" costs an hour of
// investigation at the worst possible time.
func TestHaltReasonsAreActionable(t *testing.T) {
	for _, tc := range []struct {
		name string
		live schema.Version
		want []string
	}{
		{"partial", f2, []string{"step 2 of 4", f2.Short(), from.Short(), to.Short()}},
		{"unknown", alien, []string{alien.Short(), from.Short(), to.Short()}},
		{"drifted", from, []string{"0042_add_status", from.Short()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := RecordedRunning
			if tc.name == "drifted" {
				rec = RecordedCompleted
			}
			reason := Classify(tc.live, migration(), rec).Reason
			for _, want := range tc.want {
				if !strings.Contains(reason, want) {
					t.Errorf("reason omits %q\n  got: %s", want, reason)
				}
			}
		})
	}
}

// TestSafeVerdictsAreExactlyThree pins which outcomes permit touching a
// database. Adding a verdict without deciding its safety would otherwise default
// it to unsafe silently — or, worse, to safe.
func TestSafeVerdictsAreExactlyThree(t *testing.T) {
	all := []Verdict{Proceed, SelfHeal, NothingApplied, HaltPartial, HaltDrifted, HaltUnknown}
	safe := 0
	for _, v := range all {
		if (Outcome{Verdict: v}).Safe() {
			safe++
		}
	}
	if safe != 3 {
		t.Errorf("%d verdicts allow proceeding, expected exactly 3", safe)
	}
}

func TestChain(t *testing.T) {
	chain := migration().Chain()
	want := []schema.Version{from, f1, f2, f3, to}
	if len(chain) != len(want) {
		t.Fatalf("chain length %d, want %d", len(chain), len(want))
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Errorf("chain[%d] = %s, want %s", i, chain[i], want[i])
		}
	}
}

func TestReversible(t *testing.T) {
	m := migration()
	if !m.Reversible() {
		t.Error("migration with no stated reason should be reversible")
	}
	m.IrreversibleReason = "drops column orders.legacy_status"
	if m.Reversible() {
		t.Error("migration with a stated reason must not be reversible")
	}
}

// uncheckpointed is the same migration with no per-step fingerprints — what a
// planner that has not simulated the steps produces.
func uncheckpointed() *plan.Migration {
	m := migration()
	for i := range m.Steps {
		m.Steps[i].ExpectedAfter = ""
	}
	return m
}

// endpointsOnly carries no steps at all: just the two versions it spans.
func endpointsOnly() *plan.Migration {
	return &plan.Migration{Name: "0043_endpoints", From: from, To: to}
}

// TestSafetyDoesNotDependOnCheckpoints is the property that lets reconciliation
// exist before the planner can simulate steps: the verdicts that permit touching
// a database are decided by the endpoints alone, so removing every checkpoint
// must not change a single safe/unsafe answer.
func TestSafetyDoesNotDependOnCheckpoints(t *testing.T) {
	states := []schema.Version{from, f1, f2, f3, to, alien}
	records := []Recorded{RecordedCompleted, RecordedRunning, RecordedFailed}

	for _, live := range states {
		for _, rec := range records {
			full := Classify(live, migration(), rec)
			bare := Classify(live, uncheckpointed(), rec)
			if full.Safe() != bare.Safe() {
				t.Errorf("live=%s recorded=%s: checkpoints changed safety (%v vs %v)",
					live.Short(), rec, full.Safe(), bare.Safe())
			}
		}
	}
}

// TestCheckpointsOnlyAddDetail confirms what the chain buys: the same halt, with
// the stopping point named.
func TestCheckpointsOnlyAddDetail(t *testing.T) {
	withChain := Classify(f2, migration(), RecordedRunning)
	without := Classify(f2, uncheckpointed(), RecordedRunning)

	if withChain.Verdict != HaltPartial {
		t.Errorf("with checkpoints: got %s, want halt_partial", withChain.Verdict)
	}
	if without.Verdict != HaltUnknown {
		t.Errorf("without checkpoints: got %s, want halt_unknown", without.Verdict)
	}
	if withChain.StepsApplied != 2 {
		t.Errorf("with checkpoints: %d steps applied, want 2", withChain.StepsApplied)
	}
	if !strings.Contains(withChain.Reason, "step 2 of 4") {
		t.Errorf("checkpointed reason does not locate the failure: %s", withChain.Reason)
	}
}

// TestEndpointsOnlyMigration covers a migration carrying nothing but its two
// versions — which is exactly what drift comparison produces.
func TestEndpointsOnlyMigration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     schema.Version
		recorded Recorded
		want     Verdict
	}{
		{"at target, recorded complete", to, RecordedCompleted, Proceed},
		{"at target, recorded running", to, RecordedRunning, SelfHeal},
		{"at start, recorded running", from, RecordedRunning, NothingApplied},
		{"at start, recorded complete", from, RecordedCompleted, HaltDrifted},
		{"somewhere else", alien, RecordedRunning, HaltUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.live, endpointsOnly(), tc.recorded)
			if got.Verdict != tc.want {
				t.Errorf("got %s, want %s (%s)", got.Verdict, tc.want, got.Reason)
			}
		})
	}
}

func TestChainAlwaysReachesTarget(t *testing.T) {
	for name, m := range map[string]*plan.Migration{
		"checkpointed":   migration(),
		"uncheckpointed": uncheckpointed(),
		"endpoints only": endpointsOnly(),
	} {
		chain := m.Chain()
		if chain[0] != from {
			t.Errorf("%s: chain starts at %s, want %s", name, chain[0].Short(), from.Short())
		}
		if last := chain[len(chain)-1]; last != to {
			t.Errorf("%s: chain ends at %s, want %s", name, last.Short(), to.Short())
		}
	}
}
