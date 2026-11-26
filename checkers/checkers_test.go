package checkers

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"synthusers"
)

// fakeFetch is a real fetcher over a plain HTTP client for tests.
type fakeFetch struct {
	base string
}

func (f *fakeFetch) Fetch(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return &synthusers.FetchedPage{URL: raw, Err: err}, nil
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 8192)
	buf := make([]byte, 2048)
	for {
		n, e := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if e != nil || len(body) > 512<<10 {
			break
		}
	}
	return &synthusers.FetchedPage{URL: raw, FinalURL: raw, Status: resp.StatusCode, Header: resp.Header, Body: body, Latency: latency}, nil
}

func (f *fakeFetch) FetchPage(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	return f.Fetch(ctx, raw)
}

var testTarget = synthusers.Target{ID: "t", StartURL: "http://t.test/"}

func checkPage(t *testing.T, c synthusers.Checker, page *synthusers.FetchedPage, fetch synthusers.Fetcher) []synthusers.Finding {
	t.Helper()
	return c.Check(&synthusers.CheckContext{Ctx: context.Background(), Target: &testTarget, Page: page, Fetch: fetch})
}

func findOf(findings []synthusers.Finding, typ string) *synthusers.Finding {
	for i := range findings {
		if findings[i].Type == typ && !findings[i].Pass {
			return &findings[i]
		}
	}
	return nil
}

func hasPass(findings []synthusers.Finding, typ string) bool {
	for _, f := range findings {
		if f.Type == typ && f.Pass {
			return true
		}
	}
	return false
}

func TestHTTPCheckerStatuses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	mux.HandleFunc("GET /404", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &fakeFetch{}
	c := HTTP(HTTPOptions{SlowThreshold: time.Hour}) // never slow here

	page, _ := f.FetchPage(context.Background(), srv.URL+"/500")
	findings := checkPage(t, c, page, f)
	iss := findOf(findings, synthusers.TypeHTTPStatus)
	if iss == nil || iss.Severity != synthusers.SevCritical || !strings.Contains(iss.Summary, "500") {
		t.Fatalf("want critical 500 finding, got %+v", findings)
	}

	page, _ = f.FetchPage(context.Background(), srv.URL+"/404")
	iss = findOf(checkPage(t, c, page, f), synthusers.TypeHTTPStatus)
	if iss == nil || iss.Severity != synthusers.SevWarning {
		t.Fatalf("want warning 404 finding, got %+v", iss)
	}

	page, _ = f.FetchPage(context.Background(), srv.URL+"/")
	if !hasPass(checkPage(t, c, page, f), synthusers.TypeHTTPStatus) {
		t.Fatal("healthy page must emit a pass")
	}
	if hasPass(checkPage(t, c, page, f), synthusers.TypeSlowResponse) == false {
		t.Fatal("fast page must emit slow_response pass")
	}
}

func TestHTTPCheckerSlowAndUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("slow"))
	}))
	defer srv.Close()
	c := HTTP(HTTPOptions{SlowThreshold: 50 * time.Millisecond})
	f := &fakeFetch{}
	page, _ := f.FetchPage(context.Background(), srv.URL+"/")
	iss := findOf(checkPage(t, c, page, f), synthusers.TypeSlowResponse)
	if iss == nil || !strings.Contains(iss.Summary, "slow response") {
		t.Fatalf("want slow_response finding, got %+v", iss)
	}

	// Unreachable site: closed port.
	page = &synthusers.FetchedPage{URL: "http://127.0.0.1:1/", Err: errConnectionRefused(), Status: 0}
	iss = findOf(checkPage(t, c, page, f), synthusers.TypeHTTPStatus)
	if iss == nil || iss.Severity != synthusers.SevCritical {
		t.Fatalf("unreachable site must be critical, got %+v", iss)
	}
}

func TestHTTPCheckerSSLExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	client := srv.Client()
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	c := HTTP(HTTPOptions{SlowThreshold: time.Hour})
	page := &synthusers.FetchedPage{URL: srv.URL, FinalURL: srv.URL, Status: 200, TLS: resp.TLS, Header: resp.Header}
	findings := checkPage(t, c, page, &fakeFetch{})
	// The httptest certificate is valid for a long time: expect a pass.
	if !hasPass(findings, synthusers.TypeSSLExpiry) {
		t.Fatalf("valid certificate must emit ssl pass, got %+v", findings)
	}
	if findOf(findings, synthusers.TypeSSLExpiry) != nil {
		t.Fatal("valid certificate must not warn")
	}
}

func TestAssetsChecker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/good">good</a> <a href="/missing">missing</a> <a href="https://external.test/x">ext</a>
			<img src="/img/ok.png" /><img src="/img/nope.png" />
			<script src="/missing.js"></script>
		</body></html>`))
	})
	mux.HandleFunc("GET /good", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /img/ok.png", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("png")) })
	mux.HandleFunc("GET /css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(`body { background: url(/img/ok.png); } .x { background: url(/img/also-nope.png); }`))
	})
	mux.HandleFunc("GET /csspage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><link rel="stylesheet" href="/css" /></head><body>x</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &fakeFetch{}
	page, _ := f.FetchPage(context.Background(), srv.URL+"/")
	findings := checkPage(t, Assets(AssetsOptions{}), page, f)

	if iss := findOf(findings, synthusers.TypeBrokenAsset); iss == nil || !strings.Contains(iss.URL, "/img/nope.png") {
		t.Fatalf("want broken asset for /img/nope.png, got %+v", findings)
	}
	if iss := findOf(findings, synthusers.TypeBrokenLink); iss == nil || !strings.Contains(iss.URL, "/missing") {
		t.Fatalf("want broken link for /missing, got %+v", iss)
	}

	// CSS-nested assets.
	page, _ = f.FetchPage(context.Background(), srv.URL+"/csspage")
	findings = checkPage(t, Assets(AssetsOptions{}), page, f)
	var alsoNope, okBG *synthusers.Finding
	for i := range findings {
		if strings.Contains(findings[i].URL, "/img/also-nope.png") {
			alsoNope = &findings[i]
		}
		if strings.Contains(findings[i].URL, "/img/ok.png") {
			okBG = &findings[i]
		}
	}
	if alsoNope == nil || alsoNope.Pass {
		t.Fatalf("css-referenced missing asset must fail, got %+v", findings)
	}
	if okBG == nil || !okBG.Pass {
		t.Fatalf("css-referenced existing asset must pass, got %+v", findings)
	}
}

func TestAssetsCheckerMixedContent(t *testing.T) {
	// https page referencing an http:// asset.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><img src="http://insecure.test/pixel.gif" /></body></html>`))
	}))
	defer srv.Close()
	page, err := fetchTLS(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := Assets(AssetsOptions{})
	iss := findOf(checkPage(t, c, page, &fakeFetch{}), synthusers.TypeMixedContent)
	if iss == nil || iss.Severity != synthusers.SevCritical || !strings.Contains(iss.URL, "insecure.test") {
		t.Fatalf("want mixed_content critical finding, got %+v", iss)
	}
}

func TestContentChecker(t *testing.T) {
	page := func(body string) *synthusers.FetchedPage {
		return &synthusers.FetchedPage{URL: "http://t.test/x", FinalURL: "http://t.test/x", Status: 200,
			Header: http.Header{"Content-Type": []string{"text/html"}}, Body: []byte(body)}
	}
	c := Content()

	ok := checkPage(t, c, page(`<html><head><title>Shop</title></head><body><h1>Hi</h1>content here is long enough to matter, quite long indeed, so we exceed any threshold; more words follow to pad the length of this fine page.</body></html>`), nil)
	if findOf(ok, synthusers.TypeContentFail) != nil {
		t.Fatalf("unexpected content failure: %+v", ok)
	}
	if findOf(ok, synthusers.TypeSoft404) != nil {
		t.Fatal("healthy page must not soft-404")
	}

	target := testTarget
	target.Assertions = []synthusers.Assertion{{Contains: "Add to cart"}}
	ctx := &synthusers.CheckContext{Ctx: context.Background(), Target: &target,
		Page: page(`<html><body>no button here</body></html>`), Fetch: nil}
	findings := c.Check(ctx)
	iss := findOf(findings, synthusers.TypeContentFail)
	if iss == nil || !strings.Contains(iss.Summary, "does not contain") {
		t.Fatalf("want content_mismatch, got %+v", findings)
	}

	// Soft 404 via title.
	soft := checkPage(t, c, page(`<html><head><title>Page not found - Demo</title></head><body>tiny</body></html>`), nil)
	if findOf(soft, synthusers.TypeSoft404) == nil {
		t.Fatalf("200 page titled 'not found' must soft-404, got %+v", soft)
	}
	// Pass when title is fine.
	if !hasPass(checkPage(t, c, page(`<html><head><title>Fine</title></head><body>x</body></html>`), nil), synthusers.TypeSoft404) {
		t.Fatal("fine page must emit soft404 pass")
	}
}

func TestCSSURLs(t *testing.T) {
	css := []byte(`a { background: url("/x.png") } b { background: url(http://y/z.png) } @import "inner.css"; c{background:url('/single.png')}`)
	got := cssURLs(css)
	want := []string{"/x.png", "http://y/z.png", "inner.css", "/single.png"}
	if len(got) != len(want) {
		t.Fatalf("cssURLs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cssURLs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---- small helpers used by the tests above ----

func parseDoc(body string) *html.Node {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	return doc
}

func errConnectionRefused() error {
	_, err := net.DialTimeout("tcp", "127.0.0.1:1", 50*time.Millisecond)
	return err
}

func fetchTLS(raw string) (*synthusers.FetchedPage, error) {
	req, _ := http.NewRequest("GET", raw, nil)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return &synthusers.FetchedPage{URL: raw, FinalURL: raw, Status: resp.StatusCode,
		Header: resp.Header, TLS: resp.TLS, Body: body}, nil
}
