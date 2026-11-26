package httpui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synthusers"
)

func testEngine(t *testing.T) *synthusers.Engine {
	t.Helper()
	eng := synthusers.New(synthusers.Config{DisableDelays: true, RateLimit: 500, Interval: time.Hour},
		synthusers.NewMemoryStore())
	eng.SetRunner("explorer", nopRunner{})
	ctx := context.Background()
	if err := eng.AddTarget(synthusers.Target{ID: "shop", Name: "Shop", StartURL: "https://shop.test/", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Seed one issue and one run through the store.
	_, err := eng.Store().ReportFinding(ctx, "shop", synthusers.Finding{
		Type: synthusers.TypeBrokenAsset, Severity: synthusers.SevWarning,
		URL: "https://shop.test/logo.png", Summary: "broken logo (404)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Store().SaveRun(ctx, &synthusers.Run{
		TargetID: "shop", Mode: "explorer", StartedAt: time.Now(),
		Pages: 3, Requests: 12, Persona: "chrome-windows",
	}); err != nil {
		t.Fatal(err)
	}
	return eng
}

type nopRunner struct{}

func (nopRunner) Run(ctx context.Context, req *synthusers.RunRequest) ([]synthusers.Finding, synthusers.RunSummary, error) {
	return nil, synthusers.RunSummary{}, nil
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, []byte) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	buf2 := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf2)
		buf.Write(buf2[:n])
		if e != nil {
			break
		}
	}
	return resp, []byte(buf.String())
}

func TestDashboardEndpoints(t *testing.T) {
	eng := testEngine(t)
	h := Handler(eng)
	ctx := context.Background()

	// Health.
	resp, body := get(t, h, "/healthz")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"status": "ok"`) && !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("healthz: %d %s", resp.StatusCode, body)
	}

	// UI page.
	resp, body = get(t, h, "/")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Synth Users") {
		t.Fatalf("index: %d (len %d)", resp.StatusCode, len(body))
	}
	if strings.Contains(string(body), "<script src=") {
		t.Fatal("UI must not reference external assets")
	}

	// Stats.
	resp, body = get(t, h, "/api/stats")
	if resp.StatusCode != 200 {
		t.Fatalf("stats: %d", resp.StatusCode)
	}
	var stats synthusers.Stats
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Targets != 1 || stats.OpenIssues != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	// Issues filter.
	resp, body = get(t, h, "/api/issues?status=open&severity=warning")
	var issues []synthusers.Issue
	if err := json.Unmarshal(body, &issues); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Type != synthusers.TypeBrokenAsset {
		t.Fatalf("issues = %+v", issues)
	}

	// Runs.
	_, body = get(t, h, "/api/runs?limit=10")
	var runs []synthusers.Run
	if err := json.Unmarshal(body, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Pages != 3 {
		t.Fatalf("runs = %+v", runs)
	}

	// Add target via API.
	srv := httptest.NewServer(h)
	defer srv.Close()
	addBody := `{"start_url":"https://blog.test/","name":"Blog","mode":"explorer","interval":"10m"}`
	resp2, err := http.Post(srv.URL+"/api/targets", "application/json", strings.NewReader(addBody))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("add target: %d", resp2.StatusCode)
	}
	targets, _ := eng.Targets()
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	for _, tgt := range targets {
		if tgt.ID == "blog-test" && tgt.Interval.Std() != 10*time.Minute {
			t.Fatalf("blog target interval = %v", tgt.Interval)
		}
	}

	// Invalid URL rejected.
	badResp, err := http.Post(srv.URL+"/api/targets", "application/json", strings.NewReader(`{"start_url":"notaurl"}`))
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != 400 {
		t.Fatalf("bad url must 400, got %d", badResp.StatusCode)
	}

	// Delete target.
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/api/targets/blog-test", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 200 {
		t.Fatalf("delete: %d", delResp.StatusCode)
	}
	targets, _ = eng.Targets()
	if len(targets) != 1 {
		t.Fatalf("after delete targets = %d, want 1", len(targets))
	}

	// Resolve issue.
	issues, _ = eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	resResp, err := http.Post(fmt.Sprintf("%s/api/issues/%s/resolve", srv.URL, issues[0].Fingerprint), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resResp.Body.Close()
	open, _ := eng.Store().ListIssues(ctx, synthusers.IssueFilter{Status: synthusers.StatusOpen})
	if len(open) != 0 {
		t.Fatalf("issue not resolved, %d still open", len(open))
	}

	// Unknown API route → 404 (not the SPA).
	resp, _ = get(t, h, "/api/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown api route must 404, got %d", resp.StatusCode)
	}
}

func TestLiveStream(t *testing.T) {
	eng := testEngine(t)
	srv := httptest.NewServer(Handler(eng))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	// Run after connecting: the events must arrive over the open stream.
	go eng.RunOnce(context.Background(), "shop")

	sc := bufio.NewScanner(resp.Body)
	var kinds []string
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev synthusers.LiveEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		kinds = append(kinds, ev.Kind)
		if ev.Kind == synthusers.LiveRunEnd {
			break
		}
	}
	if len(kinds) < 2 || kinds[0] != synthusers.LiveRunStart || kinds[len(kinds)-1] != synthusers.LiveRunEnd {
		t.Fatalf("stream kinds = %v (scan err %v)", kinds, sc.Err())
	}
}
