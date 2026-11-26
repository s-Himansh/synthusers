package synthusers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synthusers"
	"synthusers/checkers"
)

// TestEngineRunOnce exercises the full in-process pipeline against a small
// broken site: scheduler-less, synchronous, memory store.
func TestEngineRunOnce(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>ok</title></head><body>
		<a href="/good">good</a> <a href="/missing">missing</a>
		<img src="/img/ok.png" /><img src="/img/nope.png" /></body></html>`))
	})
	mux.HandleFunc("GET /good", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>good</title></head><body>fine</body></html>`))
	})
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	mux.HandleFunc("GET /img/ok.png", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("png"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng := synthusers.New(synthusers.Config{DisableDelays: true, RateLimit: 100, Timeout: 5 * time.Second}, synthusers.NewMemoryStore())
	eng.Use(checkers.HTTP(checkers.HTTPOptions{SlowThreshold: 10 * time.Second}))
	eng.Use(checkers.Assets(checkers.AssetsOptions{}))
	eng.Use(checkers.Content())
	eng.SetRunner("explorer", inlineRunner{pages: []string{srv.URL + "/"}})

	tgt := synthusers.Target{ID: "shop", StartURL: srv.URL + "/", Mode: synthusers.ModeExplorer, Enabled: true}
	if err := eng.AddTarget(tgt); err != nil {
		t.Fatal(err)
	}
	run, err := eng.RunOnce(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if run.NewIssues == 0 {
		t.Fatal("expected new issues from broken assets/links")
	}
	issues, _ := eng.Store().ListIssues(context.Background(), synthusers.IssueFilter{Status: synthusers.StatusOpen})
	kinds := map[string]bool{}
	for _, iss := range issues {
		kinds[iss.Type] = true
	}
	if !kinds[synthusers.TypeBrokenAsset] {
		t.Fatalf("want broken_asset issue, got %v", kinds)
	}
	if calls == 0 {
		t.Fatal("site was never visited")
	}
}

// inlineRunner is a minimal Runner used in tests.
type inlineRunner struct {
	pages []string
}

func (r inlineRunner) Run(ctx context.Context, req *synthusers.RunRequest) ([]synthusers.Finding, synthusers.RunSummary, error) {
	var findings []synthusers.Finding
	summary := synthusers.RunSummary{}
	for _, u := range r.pages {
		page, _ := req.Fetch.FetchPage(ctx, u)
		if page == nil {
			continue
		}
		summary.Pages++
		summary.Visited = append(summary.Visited, u)
		findings = append(findings, req.CheckPage(ctx, req.Target, page, req.Fetch)...)
	}
	return findings, summary, nil
}
