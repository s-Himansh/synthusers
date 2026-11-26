package explorer

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"synthusers"
)

// stubFetch is a page fetcher over a real HTTP client without probes.
type stubFetch struct{ client *http.Client }

func (s *stubFetch) fetch(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return &synthusers.FetchedPage{URL: raw, Err: err}, nil
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf)
		b.Write(buf[:n])
		if e != nil || b.Len() > 512<<10 {
			break
		}
	}
	final := raw
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &synthusers.FetchedPage{URL: raw, FinalURL: final, Status: resp.StatusCode, Body: []byte(b.String()),
		Header: resp.Header, Latency: 0}, nil
}

func (s *stubFetch) FetchPage(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	return s.fetch(ctx, raw)
}
func (s *stubFetch) Fetch(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	return s.fetch(ctx, raw)
}
func (s *stubFetch) Client() *http.Client { return s.client }
func (s *stubFetch) SubmitForm(ctx context.Context, action, method string, values url.Values) (*synthusers.FetchedPage, error) {
	return nil, fmt.Errorf("not implemented")
}

func newReq(t *testing.T, srvURL string, target *synthusers.Target, checks []synthusers.Finding) *synthusers.RunRequest {
	t.Helper()
	return &synthusers.RunRequest{
		Target:   target,
		Persona:  synthusers.Persona{Name: "test", ThinkMin: 0, ThinkMax: 0},
		Fetch:    &stubFetch{client: http.DefaultClient},
		NoDelays: true,
		Logger:   slog.Default(),
		CheckPage: func(ctx context.Context, target *synthusers.Target, page *synthusers.FetchedPage, fetch synthusers.Fetcher) []synthusers.Finding {
			return checks
		},
	}
}

func TestExplorerBudget(t *testing.T) {
	// A chain of 12 pages, each linking to the next.
	mux := http.NewServeMux()
	const n = 12
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/p%d", i)
		next := fmt.Sprintf("/p%d", i+1)
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><a href="%s">next</a></body></html>`, next)
		})
	}
	mux.HandleFunc("GET /p12", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>the end</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New(Options{MaxPages: 5, MaxDepth: 99})
	target := &synthusers.Target{ID: "t", StartURL: srv.URL + "/p0"}
	req := newReq(t, srv.URL, target, nil)
	findings, summary, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 5 {
		t.Fatalf("pages = %d, want 5 (budget)", summary.Pages)
	}
	if len(findings) != 0 {
		t.Fatalf("no findings expected, got %v", findings)
	}
}

func TestExplorerDepthLimit(t *testing.T) {
	mux := http.NewServeMux()
	for i := 0; i < 10; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("GET /p%d", i), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><a href="/p%d">next</a></body></html>`, i+1)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New(Options{MaxPages: 100, MaxDepth: 2})
	target := &synthusers.Target{ID: "t", StartURL: srv.URL + "/p0"}
	_, summary, err := r.Run(context.Background(), newReq(t, srv.URL, target, nil))
	if err != nil {
		t.Fatal(err)
	}
	// p0 (depth 0) -> p1 (1) -> p2 (2); p2 is at max depth, no deeper links enqueued.
	if summary.Pages != 3 {
		t.Fatalf("pages = %d, want 3 (depth limit)", summary.Pages)
	}
}

func TestExplorerStaysOnHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/internal">internal</a>
			<a href="http://elsewhere.test/away">external</a>
		</body></html>`))
	})
	mux.HandleFunc("GET /internal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>inside</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New(Options{MaxPages: 10, MaxDepth: 3})
	target := &synthusers.Target{ID: "t", StartURL: srv.URL}
	_, summary, err := r.Run(context.Background(), newReq(t, srv.URL, target, nil))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 2 {
		t.Fatalf("pages = %d, want 2 (home + internal; external must be skipped)", summary.Pages)
	}
	for _, v := range summary.Visited {
		if strings.Contains(v, "elsewhere.test") {
			t.Fatalf("external host must not be visited: %v", summary.Visited)
		}
	}
}

func TestExplorerFeedsCheckPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><a href="/broken">broken</a></body></html>`))
	})
	mux.HandleFunc("GET /broken", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>oops</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	canned := synthusers.Finding{Type: synthusers.TypeBrokenAsset, Severity: synthusers.SevWarning,
		URL: srv.URL + "/img.png", Summary: "broken img"}
	r := New(Options{})
	target := &synthusers.Target{ID: "t", StartURL: srv.URL}
	findings, summary, err := r.Run(context.Background(), newReq(t, srv.URL, target, []synthusers.Finding{canned}))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 { // once per visited page
		t.Fatalf("findings = %d, want 2", len(findings))
	}
	if summary.Pages != 2 {
		t.Fatalf("pages = %d, want 2", summary.Pages)
	}
}

func TestExplorerUnreachableStart(t *testing.T) {
	r := New(Options{})
	target := &synthusers.Target{ID: "t", StartURL: "http://127.0.0.1:1/"}
	_, summary, err := r.Run(context.Background(), newReq(t, target.StartURL, target, nil))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 0 {
		t.Fatalf("pages = %d, want 0", summary.Pages)
	}
}

func TestNormalizeHost(t *testing.T) {
	if normalizeHost("WWW.Example.COM.") != "example.com" {
		t.Fatal("normalizeHost failed")
	}
}
