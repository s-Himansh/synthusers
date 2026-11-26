package storepg

import (
	"context"
	"fmt"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"synthusers"
)

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	const port = 55432
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V17).
			Port(port).
			Username("test").
			Password("test").
			Database("test").
			RuntimePath(t.TempDir() + "/pgdata").
			CachePath(t.TempDir() + "/pgcache").
			Logger(nil),
	)
	if err := pg.Start(); err != nil {
		t.Skipf("embedded postgres could not start: %v", err)
	}
	defer func() { _ = pg.Stop() }()

	dsn := fmt.Sprintf("postgres://test:test@127.0.0.1:%d/test?sslmode=disable", port)
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()

	// Targets round-trip (incl. journeys and custom durations).
	in := synthusers.Target{
		ID:       "shop",
		Name:     "Shop",
		StartURL: "https://shop.test/",
		Mode:     synthusers.ModeBoth,
		Interval: synthusers.Duration(7 * time.Minute),
		MaxPages: 12,
		Journeys: []synthusers.Journey{{Name: "login", Steps: []synthusers.Step{
			{Goto: "/login"},
			{Fill: &synthusers.FillStep{Selector: "input[name=u]", Value: "demo"}},
			{Submit: "form"},
			{Expect: &synthusers.ExpectStep{Contains: "hi"}},
		}}},
		Assertions: []synthusers.Assertion{{Contains: "Add to cart"}},
		Enabled:    true,
	}
	if err := s.UpsertTarget(ctx, in); err != nil {
		t.Fatal(err)
	}
	targets, err := s.ListTargets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %d %v", len(targets), err)
	}
	got := targets[0]
	if got.Interval.Std() != 7*time.Minute || got.MaxPages != 12 ||
		len(got.Journeys) != 1 || got.Journeys[0].Steps[1].Fill.Value != "demo" {
		t.Fatalf("target round-trip mismatch: %+v", got)
	}

	// Issue lifecycle.
	f := synthusers.Finding{Type: synthusers.TypeBrokenAsset, Severity: synthusers.SevWarning,
		URL: "https://shop.test/logo.png", URLKey: "https://shop.test/logo.png", Summary: "logo 404"}
	isNew, err := s.ReportFinding(ctx, "shop", f)
	if err != nil || !isNew {
		t.Fatalf("first report: %v %v", isNew, err)
	}
	time.Sleep(5 * time.Millisecond)
	isNew, err = s.ReportFinding(ctx, "shop", f)
	if err != nil || isNew {
		t.Fatalf("second report: %v %v", isNew, err)
	}
	open, err := s.ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	if err != nil || len(open) != 1 || open[0].Occurrences != 2 {
		t.Fatalf("open issues: %+v (err %v)", open, err)
	}
	fp := open[0].Fingerprint

	// Runs.
	run := &synthusers.Run{TargetID: "shop", Mode: "both", StartedAt: time.Now(),
		Duration: synthusers.Duration(3 * time.Second), Pages: 4, Requests: 21, Persona: "chrome-linux"}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, "", 10)
	if err != nil || len(runs) != 1 || runs[0].Pages != 4 {
		t.Fatalf("runs: %+v (err %v)", runs, err)
	}

	// Stats.
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Targets != 1 || st.OpenIssues != 1 || st.RunsLast24h != 1 || st.PagesLast24h != 4 {
		t.Fatalf("stats = %+v", st)
	}
	if st.BySeverity["warning"] != 1 || st.ByType[synthusers.TypeBrokenAsset] != 1 {
		t.Fatalf("stats breakdown = %+v", st)
	}

	// Resolve.
	n, err := s.ResolveIssues(ctx, []string{fp, "nonexistent"})
	if err != nil || n != 1 {
		t.Fatalf("resolve: %d %v", n, err)
	}
	resolved, _ := s.ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusResolved})
	if len(resolved) != 1 || resolved[0].ResolvedAt == nil {
		t.Fatalf("resolved = %+v", resolved)
	}

	// Filters.
	if _, err := s.ReportFinding(ctx, "shop", synthusers.Finding{Type: synthusers.TypeSlowResponse,
		Severity: synthusers.SevCritical, URL: "https://shop.test/slow", Summary: "slow"}); err != nil {
		t.Fatal(err)
	}
	onlyCrit, _ := s.ListIssues(ctx, synthusers.IssueFilter{Severity: string(synthusers.SevCritical), Status: synthusers.StatusOpen})
	if len(onlyCrit) != 1 || onlyCrit[0].Type != synthusers.TypeSlowResponse {
		t.Fatalf("severity filter: %+v", onlyCrit)
	}
	onlyOne, _ := s.ListIssues(ctx, synthusers.IssueFilter{TargetID: "shop", Status: synthusers.StatusOpen})
	if len(onlyOne) != 1 || onlyOne[0].Type != synthusers.TypeSlowResponse {
		// broken_asset was resolved above; only slow_response is open.
		t.Fatalf("target filter: %+v", onlyOne)
	}

	// Delete target.
	if err := s.DeleteTarget(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	targets, _ = s.ListTargets(ctx)
	if len(targets) != 0 {
		t.Fatalf("delete failed: %+v", targets)
	}
}
