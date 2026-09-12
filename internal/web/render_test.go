package web

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rishabhju65/schemaver/internal/auth"
	"github.com/rishabhju65/schemaver/internal/diff"
	"github.com/rishabhju65/schemaver/internal/schema"
	"github.com/rishabhju65/schemaver/internal/store"
)

// TestRequestPageRenders executes the review page rather than merely parsing it.
//
// Parsing cannot catch a template that names a field the Go side has renamed or
// retyped — that surfaces only when the branch holding it actually renders. The
// review page is mostly optional branches: a fingerprint appears only once an
// execution finishes, an activity log only once something is worth logging, an
// earlier attempt only after a retry. A page can therefore be rendered a
// hundred times in testing and never touch the line that is broken.
//
// It matters more here than elsewhere because a failed render is not a failed
// request: the response is already partly written, so the error is appended to
// a half-finished page that still returns as if it worked. The symptom is a
// page that quietly stops early, which is easy to miss and hard to attribute.
//
// So every optional branch is populated at once.
func TestRequestPageRenders(t *testing.T) {
	s := server(t, auth.Completed(), false)
	if s.tmpl["request"] == nil {
		t.Fatal("request template missing")
	}

	now := time.Now()
	ended := now.Add(9 * time.Second)
	step := 1
	percent := 42.5
	began := now.Add(-time.Minute)

	finished := &store.ExecutionView{
		ID: 2, State: "completed", Total: 2, Done: 2,
		Started: began, Ended: &ended,
		Reason: "applied 2 statement(s)",
		Final:  schema.Version("26a5677700631f0d9d2e1b4f6c8a0e7d5b3a9c1e2f4d6b8a0c2e4f6a8b0d2f4e"),
		Steps: []store.ExecutionStep{{
			Ordinal: 1, SQL: "ALTER TABLE public.orders ADD COLUMN channel text;",
			ChangeID: "add_column:public.orders.channel",
			Started:  &began, Finished: &ended,
		}},
		Events: []store.Activity{
			{At: now, Actor: "schemaver", Level: "info", Kind: "execution.started", Message: "applying 2 statement(s)"},
			{At: now, Actor: "schemaver", Ordinal: &step, Level: "info", Kind: "step.applied", Message: "applied add_column"},
		},
	}

	// A blocked, rolled-back attempt: the branches that only a failure reaches.
	blocked := &store.ExecutionView{
		ID: 1, State: "failed", Total: 2, Done: 0,
		Started: began, Ended: &ended,
		Reason:      "step 1 failed and rolled back",
		Final:       schema.Version("c4ecc09d95cb1f0d9d2e1b4f6c8a0e7d5b3a9c1e2f4d6b8a0c2e4f6a8b0d2f4e"),
		CurrentStep: &step, CurrentStarted: &began,
		WaitEvent: "Lock:relation", BlockedBy: []int32{49445},
		BlockerQuery: "SELECT count(*) FROM orders;",
		Phase:        "building index", Percent: &percent, ObservedAt: &now,
		Events: []store.Activity{
			{At: now, Actor: "schemaver", Level: "warn", Kind: "wait.began", Message: "waiting on a lock"},
			{At: now, Actor: "schemaver", Ordinal: &step, Level: "error", Kind: "step.failed", Message: "lock timeout"},
		},
	}

	detail := &store.RequestDetail{
		RequestSummary: store.RequestSummary{
			ID: 10, Title: "bring production in line with staging",
			Author: "admin@example.com", State: "COMPLETED",
			Database: "shop_prod", Source: "shop_staging",
			CreatedAt: began, Changes: 1,
		},
		Description: "a description",
		MigrationID: 12,
		From:        blocked.Final, To: finished.Final,
		GeneratedAt: began,
		ByRisk: []diff.Change{{
			ID: "add_column:public.orders.channel", Kind: "add_column",
			Class: diff.Additive, Summary: "add orders.channel",
		}},
		Steps: []store.Step{{Ordinal: 1, SQL: "ALTER TABLE public.orders ADD COLUMN channel text;", ChangeID: "add_column:public.orders.channel", Transactional: true}},
		Approval: &store.ApprovalState{
			MigrationID: 12, ProofState: "unproven",
			ProofReason:    "no shadow server is configured",
			AdminApprovals: 1, Executable: true,
		},
		Executions: []*store.ExecutionView{finished, blocked},
		Timeline: []store.Activity{
			{At: began, Actor: "admin@example.com", Level: "info",
				Kind: "request.opened", Message: "bring production in line with staging"},
			{At: began, Actor: "admin@example.com", Level: "warn",
				Kind: "migration.generated", Message: "3 change(s), 1 of them destructive"},
		},
	}

	// Both orderings, so the panel is exercised against a live attempt as well
	// as a settled one.
	for _, name := range []string{"settled", "running"} {
		if name == "running" {
			live := *finished
			live.State, live.Ended, live.Final = "running", nil, ""
			live.CurrentStep, live.CurrentStarted = &step, &began
			detail.Executions = []*store.ExecutionView{&live, blocked}
		}
		data := map[string]any{
			"R": detail, "Refresh": name == "running",
			"CanDecide": true, "IsAuthor": false,
			"Title": detail.Title, "Nav": "requests",
			"CSRF": "token", "CanWrite": true,
		}
		if err := s.tmpl["request"].ExecuteTemplate(io.Discard, "layout", data); err != nil {
			t.Fatalf("%s: rendering the request page failed: %v", name, err)
		}
	}

	// And once more capturing the output, to confirm the log actually appears
	// rather than the template merely running without error.
	var out strings.Builder
	detail.Executions = []*store.ExecutionView{finished, blocked}
	if err := s.tmpl["request"].ExecuteTemplate(&out, "layout", map[string]any{
		"R": detail, "Title": detail.Title, "Nav": "requests", "CSRF": "t",
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	// The messages, not the event kinds: the page shows what a person reads.
	for _, want := range []string{
		"Activity", "Earlier attempts",
		"applying 2 statement(s)", "waiting on a lock", "lock timeout",
		"Timeline", "1 of them destructive", "has not been proven", "admin@example.com",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the rendered page does not mention %q", want)
		}
	}
}
