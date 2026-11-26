// Package httpui serves the dashboard of a synthusers engine: a JSON API
// over the engine and its store, plus an embedded single-page UI (no
// external assets, works offline).
package httpui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"synthusers"
)

//go:embed static/index.html
var indexHTML []byte

// Handler builds the dashboard HTTP handler for one engine.
func Handler(e *synthusers.Engine) http.Handler {
	mux := http.NewServeMux()
	s := e.Store()

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		stats, err := s.Stats(r.Context())
		writeJSON(w, stats, err)
	})

	mux.HandleFunc("GET /api/targets", func(w http.ResponseWriter, r *http.Request) {
		targets, err := e.Targets()
		if err == nil {
			sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		}
		writeJSON(w, targets, err)
	})

	mux.HandleFunc("POST /api/targets", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID         string                 `json:"id"`
			Name       string                 `json:"name"`
			StartURL   string                 `json:"start_url"`
			Mode       string                 `json:"mode"`
			Interval   synthusers.Duration    `json:"interval"`
			MaxPages   int                    `json:"max_pages"`
			MaxDepth   int                    `json:"max_depth"`
			JSSample   int                    `json:"js_sample"`
			Visitors   int                    `json:"visitors"`
			Assertions []synthusers.Assertion `json:"assertions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		u, err := url.Parse(in.StartURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			httpError(w, http.StatusBadRequest, "start_url must be an absolute http(s) URL")
			return
		}
		if in.ID == "" {
			in.ID = slugFromURL(u)
		}
		t := synthusers.Target{
			ID:         in.ID,
			Name:       firstNonEmpty(in.Name, u.Host),
			StartURL:   in.StartURL,
			Mode:       in.Mode,
			Interval:   in.Interval,
			MaxPages:   in.MaxPages,
			MaxDepth:   in.MaxDepth,
			Assertions: in.Assertions,
			Visitors:   in.Visitors,
			Enabled:    true,
		}
		t.JS.Sample = in.JSSample
		if err := e.AddTarget(t); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, t, nil)
	})

	mux.HandleFunc("DELETE /api/targets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := e.RemoveTarget(id); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]string{"deleted": id}, nil)
	})

	mux.HandleFunc("POST /api/targets/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		if err := e.TriggerRun(r.PathValue("id")); err != nil {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]string{"triggered": r.PathValue("id")}, nil)
	})

	mux.HandleFunc("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filter := synthusers.IssueFilter{
			Status:   q.Get("status"),
			Severity: q.Get("severity"),
			TargetID: q.Get("target"),
			Type:     q.Get("type"),
			Limit:    atoiDefault(q.Get("limit"), 100),
		}
		issues, err := s.ListIssues(r.Context(), filter)
		writeJSON(w, issues, err)
	})

	mux.HandleFunc("POST /api/issues/{fingerprint}/resolve", func(w http.ResponseWriter, r *http.Request) {
		fp := r.PathValue("fingerprint")
		n, err := s.ResolveIssues(r.Context(), []string{fp})
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"resolved": n}, nil)
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		runs, err := s.ListRuns(r.Context(), q.Get("target"), atoiDefault(q.Get("limit"), 50))
		writeJSON(w, runs, err)
	})

	// Server-Sent Events stream of what the synthetic users are doing now:
	// the recent backlog first, then every new event as it happens.
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{}) // long-lived stream: no write timeout
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		backlog, events, cancel := e.SubscribeLive()
		defer cancel()
		send := func(ev synthusers.LiveEvent) error {
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b); err != nil {
				return err
			}
			return rc.Flush()
		}
		for _, ev := range backlog {
			if send(ev) != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
		ping := time.NewTicker(15 * time.Second) // keeps proxies from closing an idle stream
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-events:
				if send(ev) != nil {
					return
				}
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		}
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"}, nil)
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	return logRequests(mux)
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Debug("api", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// slugFromURL turns a hostname into a target id: shop.example.com -> shop-example-com.
func slugFromURL(u *url.URL) string {
	id := strings.ToLower(u.Hostname())
	id = strings.TrimPrefix(id, "www.")
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = fmt.Sprintf("site-%d", time.Now().UnixNano()%100000)
	}
	return out
}
