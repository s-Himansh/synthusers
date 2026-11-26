package journey

import (
	"context"
	"log/slog"
	"net/http/cookiejar"

	"golang.org/x/net/html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"synthusers"
)

// journeyFetch is a full PageFetcher for tests: cookie jar, GETs and form
// submissions, mirroring the engine's runFetcher.
type journeyFetch struct {
	client *http.Client
}

func newJourneyFetch() *journeyFetch {
	jar, _ := cookiejar.New(nil)
	return &journeyFetch{client: &http.Client{Jar: jar}}
}

func (j *journeyFetch) do(ctx context.Context, method, raw string, values url.Values) (*synthusers.FetchedPage, error) {
	var req *http.Request
	var err error
	if values == nil {
		req, err = http.NewRequestWithContext(ctx, method, raw, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, raw, strings.NewReader(values.Encode()))
	}
	if err != nil {
		return nil, err
	}
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := j.client.Do(req)
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
	return &synthusers.FetchedPage{
		URL: raw, FinalURL: final, Status: resp.StatusCode,
		Header: resp.Header, Body: []byte(b.String()),
	}, nil
}

func (j *journeyFetch) FetchPage(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	return j.do(ctx, "GET", raw, nil)
}

func (j *journeyFetch) Fetch(ctx context.Context, raw string) (*synthusers.FetchedPage, error) {
	return j.do(ctx, "GET", raw, nil)
}

func (j *journeyFetch) SubmitForm(ctx context.Context, action, method string, values url.Values) (*synthusers.FetchedPage, error) {
	if strings.EqualFold(method, "GET") {
		u, _ := url.Parse(action)
		q := u.Query()
		for k, vs := range values {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		u.RawQuery = q.Encode()
		return j.do(ctx, "GET", u.String(), nil)
	}
	return j.do(ctx, "POST", action, values)
}

func (j *journeyFetch) Client() *http.Client { return j.client }

// loginSite is a demo site with a real (cookie-based) login.
func loginSite(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	sessions := map[string]bool{}
	cookie := func(w http.ResponseWriter, id string) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: id, Path: "/"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><a href="/login">Login</a><a href="/logout">out</a></body></html>`))
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Sign in</title></head><body>
		<form action="/login" method="post">
		<input type="hidden" name="csrf" value="tok" />
		<input type="text" name="username" />
		<input type="password" name="password" />
		<button type="submit">Sign in</button>
		</form></body></html>`))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("username") == "demo" && r.FormValue("password") == "hunter2" {
			id := "sid-1"
			mu.Lock()
			sessions[id] = true
			mu.Unlock()
			cookie(w, id)
			http.Redirect(w, r, "/account", http.StatusSeeOther)
			return
		}
		w.Write([]byte(`<html><body><p>Invalid credentials</p></body></html>`))
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("sid")
		if err != nil || !sessions[c.Value] {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Write([]byte(`<html><body><h1>Welcome back, demo user!</h1><a href="/logout">Sign out</a></body></html>`))
	})
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(sessions, "sid-1")
		mu.Unlock()
		w.Write([]byte(`<html><body>Signed out. <a href="/">Home</a></body></html>`))
	})
	return httptest.NewServer(mux)
}

func parseDocStr(body string) *html.Node {
	return parseDoc([]byte(body))
}

func runJourney(t *testing.T, base string, j synthusers.Journey) ([]synthusers.Finding, synthusers.RunSummary) {
	t.Helper()
	r := New()
	target := &synthusers.Target{ID: "t", StartURL: base, Journeys: []synthusers.Journey{j}}
	req := &synthusers.RunRequest{
		Target:   target,
		Persona:  synthusers.Persona{Name: "test", ThinkMin: 0, ThinkMax: 0},
		Fetch:    newJourneyFetch(),
		NoDelays: true,
		Logger:   slog.Default(),
		CheckPage: func(ctx context.Context, target *synthusers.Target, page *synthusers.FetchedPage, fetch synthusers.Fetcher) []synthusers.Finding {
			return nil
		},
	}
	findings, summary, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return findings, summary
}

func journeyFailed(findings []synthusers.Finding) *synthusers.Finding {
	for i := range findings {
		if findings[i].Type == synthusers.TypeJourneyFail && !findings[i].Pass {
			return &findings[i]
		}
	}
	return nil
}

func journeyPassed(findings []synthusers.Finding) bool {
	for _, f := range findings {
		if f.Type == synthusers.TypeJourneyFail && f.Pass {
			return true
		}
	}
	return false
}

func TestJourneyLoginFlow(t *testing.T) {
	srv := loginSite(t)
	defer srv.Close()

	j := synthusers.Journey{Name: "login-logout", Steps: []synthusers.Step{
		{Goto: "/login"},
		{Expect: &synthusers.ExpectStep{Contains: "Sign in", Status: 200}},
		{Fill: &synthusers.FillStep{Selector: "input[name=username]", Value: "demo"}},
		{Fill: &synthusers.FillStep{Selector: "input[name=password]", Value: "hunter2"}},
		{Submit: "form[action=/login]"},
		{Expect: &synthusers.ExpectStep{Contains: "Welcome back"}},
		{Click: "a[href=/logout]"},
		{Expect: &synthusers.ExpectStep{Contains: "Signed out"}},
	}}
	findings, summary := runJourney(t, srv.URL, j)
	if journeyFailed(findings) != nil {
		t.Fatalf("journey should pass, got: %+v", journeyFailed(findings))
	}
	if !journeyPassed(findings) {
		t.Fatalf("missing pass finding: %+v", findings)
	}
	found := false
	for _, v := range summary.Visited {
		if strings.Contains(v, "/account") {
			found = true
		}
	}
	if !found {
		t.Fatalf("journey must have visited /account, visited: %v", summary.Visited)
	}
}

func TestJourneyBadPasswordFails(t *testing.T) {
	srv := loginSite(t)
	defer srv.Close()

	j := synthusers.Journey{Name: "wrong-password", Steps: []synthusers.Step{
		{Goto: "/login"},
		{Fill: &synthusers.FillStep{Selector: "input[name=username]", Value: "demo"}},
		{Fill: &synthusers.FillStep{Selector: "input[name=password]", Value: "letmein"}},
		{Submit: "form[action=/login]"},
		{Expect: &synthusers.ExpectStep{Contains: "Welcome back"}},
	}}
	findings, _ := runJourney(t, srv.URL, j)
	iss := journeyFailed(findings)
	if iss == nil || !strings.Contains(iss.Summary, "expectation unmet") {
		t.Fatalf("want journey_failed for bad password, got %+v", findings)
	}
	if !strings.Contains(iss.Detail, "wrong-password") || !strings.Contains(iss.Detail, "failed here") {
		t.Fatalf("failure detail must describe the journey: %q", iss.Detail)
	}
}

func TestJourneyMissingSelectorFails(t *testing.T) {
	srv := loginSite(t)
	defer srv.Close()

	j := synthusers.Journey{Name: "missing", Steps: []synthusers.Step{
		{Goto: "/login"},
		{Fill: &synthusers.FillStep{Selector: "input[name=doesnotexist]", Value: "x"}},
	}}
	findings, _ := runJourney(t, srv.URL, j)
	iss := journeyFailed(findings)
	if iss == nil || !strings.Contains(iss.Summary, "no element matches") {
		t.Fatalf("want no-element failure, got %+v", iss)
	}
}

func TestSelectorEngine(t *testing.T) {
	doc := parseDocStr(`<html><body>
		<form action="/login" method="post" id="login-form">
			<input type="hidden" name="csrf" value="tok" />
			<input type="text" name="user" class="field" />
			<input type="password" name="pass" />
			<button type="submit">Go</button>
		</form>
		<a href="/logout" class="btn primary">out</a>
	</body></html>`)

	if n := QueryFirst(doc, "form#login-form"); n == nil {
		t.Fatal("form#login-form not found")
	}
	if n := QueryFirst(doc, "form[action*=/login] input[name=user]"); n == nil {
		t.Fatal("descendant [attr*=] selector failed")
	}
	if n := QueryFirst(doc, "input.field"); n == nil {
		t.Fatal("class selector failed")
	}
	if n := QueryFirst(doc, "a.primary[href*=/logout]"); n == nil {
		t.Fatal("compound attrs on tag failed")
	}
	if n := QueryFirst(doc, "input[name=doesnotexist]"); n != nil {
		t.Fatal("non-matching selector must be nil")
	}
	if n := QueryFirst(doc, "div >>> broken selector"); n != nil {
		t.Fatal("invalid selector must be nil")
	}
	if all := QueryAll(doc, "form input"); len(all) != 3 {
		t.Fatalf("form input count = %d, want 3", len(all))
	}
}

func TestCollectFormValues(t *testing.T) {
	doc := parseDocStr(`<html><body>
	<form action="/go" method="post">
		<input type="hidden" name="csrf" value="tok" />
		<input type="text" name="user" value="default" />
		<input type="text" name="over" value="default" />
		<input type="checkbox" name="remember" checked value="yes" />
		<input type="checkbox" name="unchecked" value="yes" />
		<select name="color"><option value="r">Red</option><option value="g" selected>Green</option></select>
		<textarea name="bio">hello</textarea>
	</form>
	</body></html>`)
	form := QueryFirst(doc, "form")
	v := collectFormValues(form, map[string]string{"over": "filled", "color": "Red"})
	if v.Get("csrf") != "tok" {
		t.Fatalf("csrf = %q", v.Get("csrf"))
	}
	if v.Get("user") != "default" {
		t.Fatalf("user = %q", v.Get("user"))
	}
	if v.Get("over") != "filled" {
		t.Fatalf("filled value must win, got %q", v.Get("over"))
	}
	if v.Get("remember") != "yes" || v.Get("unchecked") != "" {
		t.Fatalf("checkbox state wrong: %v", v)
	}
	if v.Get("color") != "r" {
		// The journey filled "Red" (visible text) -> value "r" wins over the
		// markup's selected="g" option, exactly like a browser.
		t.Fatalf("filled option must win, got %q", v.Get("color"))
	}
	if v.Get("bio") != "hello" {
		t.Fatalf("textarea = %q", v.Get("bio"))
	}
}
