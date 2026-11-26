package synthusers_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synthusers"
	"synthusers/checkers"
	"synthusers/examples/brokensite"
	"synthusers/explorer"
	"synthusers/httpui"
	"synthusers/journey"
)

// startBrokensite runs the demo site on two random free ports and returns
// the https base URL and a cleanup func.
func insecureCfg() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

func startBrokensite(t *testing.T) (string, func()) {
	t.Helper()
	cert, err := brokensite.SelfSignedCert(10) // expires in 10 days -> ssl_expiry warning
	if err != nil {
		t.Fatal(err)
	}
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpPort := httpLn.Addr().(*net.TCPAddr).Port
	httpsPort := httpsLn.Addr().(*net.TCPAddr).Port
	origin := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	httpSrv := &http.Server{Handler: brokensite.NewHandler(brokensite.Options{})}
	go func() { _ = httpSrv.Serve(httpLn) }()

	httpsSrv := &http.Server{
		Handler: brokensite.NewHandler(brokensite.Options{HTTPOrigin: origin}),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() { _ = httpsSrv.ServeTLS(httpsLn, "", "") }()

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = httpsSrv.Shutdown(ctx)
	}
	return fmt.Sprintf("https://127.0.0.1:%d", httpsPort), cleanup
}

func TestE2EFullStack(t *testing.T) {
	base, cleanup := startBrokensite(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	eng := synthusers.New(synthusers.Config{
		DisableDelays: true,
		RateLimit:     100,
		Timeout:       20 * time.Second,
		RunTimeout:    3 * time.Minute,
	}, synthusers.NewMemoryStore())
	eng.Use(checkers.HTTP(checkers.HTTPOptions{SlowThreshold: 3 * time.Second}))
	eng.Use(checkers.Assets(checkers.AssetsOptions{}))
	eng.Use(checkers.Content())
	eng.SetRunner("explorer", explorer.New(explorer.Options{MaxPages: 15, MaxDepth: 3}))
	eng.SetRunner("journey", journey.New())

	target := synthusers.Target{
		ID:                 "demo-shop",
		Name:               "Demo Shop",
		StartURL:           base + "/",
		Mode:               synthusers.ModeBoth,
		Enabled:            true,
		InsecureSkipVerify: true,
		MaxPages:           15,
		MaxDepth:           3,
		Assertions:         []synthusers.Assertion{{Contains: "Demo Shop"}},
		Journeys: []synthusers.Journey{
			{
				Name: "login-logout",
				Steps: []synthusers.Step{
					{Goto: "/login"},
					{Expect: &synthusers.ExpectStep{Contains: "Sign in"}},
					{Fill: &synthusers.FillStep{Selector: "input[name=username]", Value: "demo"}},
					{Fill: &synthusers.FillStep{Selector: "input[name=password]", Value: "hunter2"}},
					{Submit: "form[action=/login]"},
					{Expect: &synthusers.ExpectStep{Contains: "Welcome back"}},
					{Click: "a[href=/logout]"},
					{Expect: &synthusers.ExpectStep{Contains: "Demo Shop"}},
				},
			},
			{
				Name: "wrong-password",
				Steps: []synthusers.Step{
					{Goto: "/login"},
					{Fill: &synthusers.FillStep{Selector: "input[name=username]", Value: "demo"}},
					{Fill: &synthusers.FillStep{Selector: "input[name=password]", Value: "letmein"}},
					{Submit: "form[action=/login]"},
					{Expect: &synthusers.ExpectStep{Contains: "Welcome back"}}, // fails on purpose
				},
			},
		},
	}
	if err := eng.AddTarget(target); err != nil {
		t.Fatal(err)
	}

	run, err := eng.RunOnce(ctx, "demo-shop")
	if err != nil {
		t.Fatal(err)
	}
	if run.Pages == 0 || run.Requests == 0 {
		t.Fatalf("run did no work: %+v", run)
	}
	if run.Error != "" {
		t.Fatalf("run error: %s", run.Error)
	}

	issues, err := eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]*synthusers.Issue{}
	for i := range issues {
		open[issues[i].Type] = &issues[i]
	}
	want := []string{
		synthusers.TypeBrokenAsset,  // missing-logo.png, app.js, broken.jpg, css nope-bg.png
		synthusers.TypeBrokenLink,   // /gone and the unreachable external link
		synthusers.TypeMixedContent, // http:// pixel.gif on the https page
		synthusers.TypeHTTPStatus,   // /broken-500 -> 500
		synthusers.TypeSlowResponse, // /slow takes 6s
		synthusers.TypeSoft404,      // /products/3 says "not found" with HTTP 200
		synthusers.TypeSSLExpiry,    // certificate valid for 10 days only
		synthusers.TypeJourneyFail,  // wrong-password journey
	}
	for _, typ := range want {
		iss, ok := open[typ]
		if !ok {
			t.Errorf("missing %s issue among %d open issues (%v)", typ, len(issues), openKeys(open))
			continue
		}
		t.Logf("found %s: %s", typ, iss.Summary)
	}
	if t.Failed() {
		t.FailNow()
	}
	if len(issues) < 8 {
		t.Fatalf("expected at least 8 open issues, got %d", len(issues))
	}

	// The dashboard reads the same store.
	uiSrv := httptest.NewServer(httpui.Handler(eng))
	defer uiSrv.Close()
	respBody, err := fetchJSON(uiSrv.URL + "/api/issues?status=open&limit=200")
	if err != nil {
		t.Fatal(err)
	}
	var apiIssues []synthusers.Issue
	if err := json.Unmarshal(respBody, &apiIssues); err != nil {
		t.Fatal(err)
	}
	if len(apiIssues) != len(issues) {
		t.Fatalf("api issues = %d, want %d", len(apiIssues), len(issues))
	}
	statsBody, err := fetchJSON(uiSrv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	var stats synthusers.Stats
	if err := json.Unmarshal(statsBody, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.OpenIssues != len(issues) || stats.Targets != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.BySeverity["critical"] < 1 {
		t.Fatalf("expected critical issues in stats: %+v", stats)
	}
}

func openKeys(m map[string]*synthusers.Issue) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func fetchJSON(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	var b strings.Builder
	buf := make([]byte, 8192)
	for {
		n, e := resp.Body.Read(buf)
		b.Write(buf[:n])
		if e != nil {
			break
		}
	}
	return []byte(b.String()), nil
}

// TestE2EResolutionCycle verifies the lifecycle end to end: an issue opens,
// stays open while broken, and auto-resolves once the site is fixed.
func TestE2EResolutionCycle(t *testing.T) {
	var broken = int32(1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Fine</title></head><body><img src="/logo.png" /></body></html>`))
	})
	mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
		if broken == 1 {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Write([]byte("png"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng := synthusers.New(synthusers.Config{DisableDelays: true, RateLimit: 100, Timeout: 10 * time.Second},
		synthusers.NewMemoryStore())
	eng.Use(checkers.Assets(checkers.AssetsOptions{}))
	eng.SetRunner("explorer", explorer.New(explorer.Options{MaxPages: 2}))

	tgt := synthusers.Target{ID: "r", StartURL: srv.URL + "/", Mode: synthusers.ModeExplorer, Enabled: true}
	if err := eng.AddTarget(tgt); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First run: broken logo -> issue open.
	if _, err := eng.RunOnce(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	open, _ := eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	if len(open) != 1 || open[0].Type != synthusers.TypeBrokenAsset {
		t.Fatalf("want 1 open broken_asset, got %+v", open)
	}
	fp := open[0].Fingerprint

	// Second run, still broken: same issue, occurrences bump, not duplicated.
	if _, err := eng.RunOnce(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	open, _ = eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	if len(open) != 1 || open[0].Occurrences != 2 {
		t.Fatalf("occurrences must bump: %+v", open)
	}

	// Third run after the fix: the issue auto-resolves.
	broken = 0
	if _, err := eng.RunOnce(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	open, _ = eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	if len(open) != 0 {
		t.Fatalf("issue must auto-resolve, still open: %+v", open)
	}
	resolved, _ := eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusResolved})
	if len(resolved) != 1 || resolved[0].Fingerprint != fp {
		t.Fatalf("resolved set wrong: %+v", resolved)
	}
}
