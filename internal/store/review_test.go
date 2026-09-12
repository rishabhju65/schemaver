package store

import (
	"strings"
	"testing"

	"github.com/rishabhju65/schemaver/internal/auth"
)

// TestOnlyAdministratorApprovalsCount is the rule: an approval from anyone else
// is recorded but does not open the gate.
func TestOnlyAdministratorApprovalsCount(t *testing.T) {
	for _, tc := range []struct {
		role    auth.Role
		verdict string
		counts  bool
	}{
		{auth.Admin, "approve", true},
		{auth.Operator, "approve", false},
		{auth.Viewer, "approve", false},
		{auth.Admin, "request_changes", false},
		{auth.Admin, "reject", false},
	} {
		d := Decision{ReviewerRole: tc.role, Verdict: tc.verdict}
		if d.Approves() != tc.counts {
			t.Errorf("%s %s: counts=%v, want %v", tc.role, tc.verdict, d.Approves(), tc.counts)
		}
	}
}

func TestBlockingVerdicts(t *testing.T) {
	for verdict, blocks := range map[string]bool{
		"approve": false, "request_changes": true, "reject": true,
	} {
		if got := (Decision{Verdict: verdict}).Blocks(); got != blocks {
			t.Errorf("%s: blocks=%v, want %v", verdict, got, blocks)
		}
	}
}

// TestGateRefusesWithoutAdministratorApproval covers the stated rule directly.
func TestGateRefusesWithoutAdministratorApproval(t *testing.T) {
	st := &ApprovalState{RevertWritten: true}
	ok, reason := st.evaluate()
	if ok {
		t.Error("executable with no approval at all")
	}
	if !strings.Contains(reason, "administrator") {
		t.Errorf("reason does not name what is missing: %s", reason)
	}

	st.AdminApprovals = 1
	if ok, reason := st.evaluate(); !ok {
		t.Errorf("one administrator approval should be enough: %s", reason)
	}
}

// TestBlockingBeatsApproval checks a rejection is not outvoted by an approval.
// Review is a veto, not a poll.
func TestBlockingBeatsApproval(t *testing.T) {
	st := &ApprovalState{RevertWritten: true, AdminApprovals: 3, Blocking: 1}
	ok, reason := st.evaluate()
	if ok {
		t.Error("three approvals outvoted a rejection")
	}
	if !strings.Contains(reason, "requested changes") && !strings.Contains(reason, "rejected") {
		t.Errorf("reason is unclear: %s", reason)
	}
}

// TestUnansweredRenamesBlockExecution is the data-loss guard. An approval cannot
// substitute for answering whether a column is being renamed or destroyed.
func TestUnansweredRenamesBlockExecution(t *testing.T) {
	st := &ApprovalState{RevertWritten: true, AdminApprovals: 2, UnansweredRenames: 1}
	ok, reason := st.evaluate()
	if ok {
		t.Error("executable with an unanswered rename question")
	}
	for _, want := range []string{"rename", "destroys data"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason omits %q: %s", want, reason)
		}
	}
}

// TestReasonOrderIsMostActionableFirst checks the refusal names the thing to fix
// next, not merely the first thing wrong. A reviewer's objection is more urgent
// than a missing approval, because the approval would be pointless until it is
// resolved.
func TestReasonOrderIsMostActionableFirst(t *testing.T) {
	everything := &ApprovalState{RevertWritten: true, Blocking: 1, UnansweredRenames: 2, AdminApprovals: 0}
	_, reason := everything.evaluate()
	if !strings.Contains(reason, "requested changes") && !strings.Contains(reason, "rejected") {
		t.Errorf("with several problems, the reviewer objection should lead: %s", reason)
	}

	noBlock := &ApprovalState{RevertWritten: true, UnansweredRenames: 2, AdminApprovals: 0}
	if _, reason := noBlock.evaluate(); !strings.Contains(reason, "rename") {
		t.Errorf("renames should lead over a missing approval: %s", reason)
	}
}

// TestExecutableRequiresEverything is the whole gate, asserted in one place so a
// future condition cannot be added without deciding how it interacts.
func TestExecutableRequiresEverything(t *testing.T) {
	ready := &ApprovalState{RevertWritten: true, AdminApprovals: 1}
	if ok, reason := ready.evaluate(); !ok {
		t.Fatalf("a clean state should be executable: %s", reason)
	}

	for name, st := range map[string]*ApprovalState{
		"no approval":       {RevertWritten: true},
		"blocked":           {RevertWritten: true, AdminApprovals: 1, Blocking: 1},
		"unanswered rename": {RevertWritten: true, AdminApprovals: 1, UnansweredRenames: 1},
		// The author's half, which no amount of approving substitutes for.
		"no way back": {AdminApprovals: 1},
	} {
		if ok, _ := st.evaluate(); ok {
			t.Errorf("%s: should not be executable", name)
		}
	}
}
