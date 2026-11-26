package jserror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"synthusers"
)

// TestHealthyDetection only asserts the graceful-degradation contract: with
// no browser available Healthy() must be false and CheckURL must fail
// without panicking. On machines with Chrome installed the full-path test
// below runs.
func TestNoBrowserGraceful(t *testing.T) {
	c := New(Options{ExecPath: "/nonexistent/chrome-binary-xyz"})
	if c.Healthy() {
		t.Skip("a browser was found; full-path test covers real behavior")
	}
	_, err := c.CheckURL(context.Background(), &synthusers.Target{ID: "t"}, "http://127.0.0.1:1/", synthusers.Persona{})
	if err == nil {
		t.Fatal("CheckURL without a browser must error")
	}
}

// TestCheckURLRealBrowser exercises the real browser path when one exists.
func TestCheckURLRealBrowser(t *testing.T) {
	c := New(Options{})
	if !c.Healthy() {
		t.Skip("no local Chrome/Chromium on PATH")
	}
	defer c.Close()

	page := `<html><body><script>
	console.error("boom: widget failed");
	setTimeout(function(){ throw new TypeError("async kaboom"); }, 50);
	</script></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
	defer srv.Close()

	findings, err := c.CheckURL(context.Background(), &synthusers.Target{ID: "t", JS: synthusers.JSOptions{Sample: 1}},
		srv.URL, synthusers.Persona{Name: "chrome-windows", UserAgent: "SynthUsersTest/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	var consoleErr, exception bool
	for _, f := range findings {
		if f.Pass {
			continue
		}
		if f.Severity == synthusers.SevCritical {
			exception = true
		}
		if f.Severity == synthusers.SevWarning {
			consoleErr = true
		}
	}
	if !consoleErr {
		t.Errorf("want console.error finding, got %+v", findings)
	}
	if !exception {
		t.Errorf("want uncaught exception finding, got %+v", findings)
	}
}
