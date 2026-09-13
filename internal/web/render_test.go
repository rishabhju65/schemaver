package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
			"CanDecide": true, "IsAuthor": false, "CanEdit": true,
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
		"CanEdit": true,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	// The messages, not the event kinds: the page shows what a person reads.
	for _, want := range []string{
		"Activity", "Earlier attempts",
		"applying 2 statement(s)", "waiting on a lock", "lock timeout",
		"Timeline", "1 of them destructive", "has not been proven",
		"withdraws every approval", "name=\"sql\"", "admin@example.com",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the rendered page does not mention %q", want)
		}
	}
}

// TestEmptyStatesOfferAWayForward executes the pages a first-time visitor sees
// before anything exists.
//
// An empty table that says only "nothing here" is where onboarding goes to die:
// registering a server lives behind Servers, which is not where anyone would
// look for it, and the control that gets there is hidden entirely in the
// organisation-wide view. A control that is simply absent reads as a missing
// feature rather than as a deliberate restriction, so each case has to say
// which one it is.
// TestPopulatedFleetStillOffersOnboarding guards the regression that made
// onboarding unreachable in practice.
//
// The link to add databases lived only in the empty-state row, so it vanished
// the moment a deployment had one database — which is every deployment past its
// first few minutes, and exactly when somebody goes looking for how to add the
// second. An empty-state test passes throughout, because the branch it checks
// is the one branch that was never broken.
func TestPopulatedFleetStillOffersOnboarding(t *testing.T) {
	s := server(t, auth.Completed(), false)

	rows := []store.DatabaseRow{{ID: 1, Name: "shop_prod", Instance: "db:5432", Managed: true}}
	var out strings.Builder
	if err := s.tmpl["fleet"].ExecuteTemplate(&out, "layout", map[string]any{
		"Rows": rows, "CanWrite": true,
		"Title": "Fleet", "Nav": "fleet", "CSRF": "t",
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	// Asserted on the control's own words, not on the href. The nav links to
	// /instances on every page, so matching the path would pass whether or not
	// the page offered anything at all — which is how the first version of this
	// test passed against the very regression it was written for.
	if !strings.Contains(out.String(), "Add databases") {
		t.Error("a fleet with databases in it offers no way to add another; " +
			"onboarding is not a first-run task")
	}

	// A reader who cannot write is not offered the control — but is told why,
	// on a populated fleet as well as an empty one. Silence where a control
	// would be reads as a missing feature, which is the whole defect this test
	// exists for, and it has a separate branch per reason.
	for _, c := range []struct {
		name    string
		orgWide bool
		want    string
	}{
		{"organisation-wide", true, "read-only"},
		{"not an administrator", false, "Only a project administrator"},
	} {
		var ro strings.Builder
		if err := s.tmpl["fleet"].ExecuteTemplate(&ro, "layout", map[string]any{
			"Rows": rows, "CanWrite": false, "OrgWide": c.orgWide,
			"Title": "Fleet", "Nav": "fleet", "CSRF": "t",
		}); err != nil {
			t.Fatalf("render %s: %v", c.name, err)
		}
		if strings.Contains(ro.String(), "Add databases") {
			t.Errorf("%s: offered a control they cannot use", c.name)
		}
		if !strings.Contains(ro.String(), c.want) {
			t.Errorf("%s: no explanation where the control would be; "+
				"an absent control reads as a missing feature", c.name)
		}
	}
}

func TestEmptyStatesOfferAWayForward(t *testing.T) {
	s := server(t, auth.Completed(), false)

	for _, c := range []struct {
		page     string
		canWrite bool
		orgWide  bool
		want     string
	}{
		{"fleet", true, false, "/instances/new"},
		{"fleet", false, true, "read-only"},
		{"fleet", false, false, "Ask a project administrator"},
		{"instances", true, false, "/instances/new"},
		{"instances", false, true, "read-only"},
		{"instances", false, false, "Only a project administrator"},
	} {
		var out strings.Builder
		err := s.tmpl[c.page].ExecuteTemplate(&out, "layout", map[string]any{
			"Rows": nil, "Entries": nil,
			"CanWrite": c.canWrite, "OrgWide": c.orgWide,
			"Title": c.page, "Nav": c.page, "CSRF": "t",
		})
		if err != nil {
			t.Fatalf("%s (canWrite=%v orgWide=%v): %v", c.page, c.canWrite, c.orgWide, err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("the empty %s page (canWrite=%v orgWide=%v) does not mention %q",
				c.page, c.canWrite, c.orgWide, c.want)
		}
	}
}

// TestAddServerFormShowsTheEngineAsAChoice checks the two fields that look
// alike and are not.
//
// "Engine" is what kind of server this is, and has one option because D-003
// restricts the product to PostgreSQL for now. "Database to connect to" is the
// database opened first in order to enumerate the others, and must stay free
// text: hosted Postgres rarely calls it postgres — Neon uses neondb,
// DigitalOcean defaultdb — so constraining it to a fixed value would make the
// most likely servers to onboard impossible to onboard.
func TestAddServerFormShowsTheEngineAsAChoice(t *testing.T) {
	s := server(t, auth.Completed(), false)

	var out strings.Builder
	if err := s.tmpl["instance_new"].ExecuteTemplate(&out, "layout", map[string]any{
		"Form":         connectionForm{Port: "5432", Database: "postgres", TLSMode: "require", Engine: "postgres"},
		"TLSModes":     tlsModes,
		"Engines":      engines,
		"EngineLabels": engineLabels,
		"Title":        "Add a server", "Nav": "instances", "CSRF": "t",
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	page := out.String()

	if !strings.Contains(page, `<select name="engine"`) {
		t.Error("the engine is not offered as a choice; a limit nobody can see " +
			"is discovered by having a server rejected")
	}
	if !strings.Contains(page, "PostgreSQL") {
		t.Error("the engine option is not labelled as it is written")
	}
	for _, other := range []string{"MySQL", "MariaDB", "SQL Server"} {
		if strings.Contains(page, other) {
			t.Errorf("%s is offered and cannot work: nothing introspects it", other)
		}
	}

	// The bootstrap database stays free text. This is the guard, not decoration:
	// making it a fixed list would lock out every hosted Postgres that does not
	// name its first database "postgres".
	if !strings.Contains(page, `<input name="database"`) {
		t.Error("the database to connect to is no longer free text; a server " +
			"whose first database is neondb or defaultdb could not be registered")
	}
}

// TestUnknownEngineFallsBackRatherThanPassingThrough covers a POST that did not
// come from the form.
func TestUnknownEngineFallsBackRatherThanPassingThrough(t *testing.T) {
	for _, submitted := range []string{"mysql", "", "postgres; DROP TABLE x", "POSTGRES"} {
		r := httptest.NewRequest(http.MethodPost, "/instances/new",
			strings.NewReader("engine="+url.QueryEscape(submitted)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := formFrom(r).Engine; got != "postgres" {
			t.Errorf("engine %q was carried forward as %q, want postgres", submitted, got)
		}
	}
}
