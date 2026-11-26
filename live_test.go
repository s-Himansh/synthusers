package synthusers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"synthusers"
	"synthusers/checkers"
)

// TestLiveEvents checks a run streams start → page → probes → issue → end,
// all tagged with one run id, and that a late subscriber gets the backlog.
func TestLiveEvents(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><img src="/nope.png"></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng := synthusers.New(synthusers.Config{DisableDelays: true, RateLimit: 100, Timeout: 5 * time.Second}, synthusers.NewMemoryStore())
	eng.Use(checkers.Assets(checkers.AssetsOptions{}))
	eng.SetRunner("explorer", inlineRunner{pages: []string{srv.URL + "/"}})
	if err := eng.AddTarget(synthusers.Target{ID: "shop", StartURL: srv.URL + "/", Mode: synthusers.ModeExplorer, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	_, live, cancel := eng.SubscribeLive()
	defer cancel()
	if _, err := eng.RunOnce(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}

	var got []synthusers.LiveEvent
	for done := false; !done; {
		select {
		case ev := <-live:
			got = append(got, ev)
			done = ev.Kind == synthusers.LiveRunEnd
		case <-time.After(2 * time.Second):
			t.Fatalf("no run_end; got %+v", got)
		}
	}
	kinds := map[string]int{}
	for i, ev := range got {
		kinds[ev.Kind]++
		if ev.RunID != got[0].RunID || ev.RunID == "" {
			t.Fatalf("event %d has run id %q, want %q", i, ev.RunID, got[0].RunID)
		}
		if i > 0 && ev.Seq <= got[i-1].Seq {
			t.Fatalf("seq not increasing at %d", i)
		}
	}
	if got[0].Kind != synthusers.LiveRunStart || kinds[synthusers.LivePage] != 1 || kinds[synthusers.LiveProbe] == 0 || kinds[synthusers.LiveIssue] == 0 {
		t.Fatalf("unexpected event mix %v", kinds)
	}
	if end := got[len(got)-1]; end.Run == nil || end.Run.Pages != 1 {
		t.Fatalf("run_end missing summary: %+v", end)
	}

	backlog, _, cancel2 := eng.SubscribeLive()
	defer cancel2()
	if len(backlog) != len(got) {
		t.Fatalf("backlog has %d events, want %d", len(backlog), len(got))
	}
}

// TestCrowdVisitors checks Target.Visitors sends that many distinct
// personas concurrently, each with its own run record and live run id,
// and that the crowd is capped at the number of personas.
func TestCrowdVisitors(t *testing.T) {
	var mu sync.Mutex
	inside, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inside++
		peak = max(peak, inside)
		mu.Unlock()
		time.Sleep(150 * time.Millisecond) // long enough for visitors to overlap
		mu.Lock()
		inside--
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>hi</body></html>`))
	}))
	defer srv.Close()

	eng := synthusers.New(synthusers.Config{DisableDelays: true, RateLimit: 100, Timeout: 5 * time.Second}, synthusers.NewMemoryStore())
	eng.SetRunner("explorer", inlineRunner{pages: []string{srv.URL + "/"}})
	ctx := context.Background()
	for _, tc := range []struct {
		id       string
		visitors int
		wantRuns int
	}{{"crowd", 3, 3}, {"huge", 50, len(eng.Personas())}} {
		if err := eng.AddTarget(synthusers.Target{ID: tc.id, StartURL: srv.URL + "/", Mode: synthusers.ModeExplorer, Enabled: true, Visitors: tc.visitors}); err != nil {
			t.Fatal(err)
		}
		_, live, cancel := eng.SubscribeLive()
		if _, err := eng.RunOnce(ctx, tc.id); err != nil {
			t.Fatal(err)
		}
		runIDs := map[string]bool{}
		for ends := 0; ends < tc.wantRuns; {
			select {
			case ev := <-live:
				if ev.TargetID != tc.id {
					continue
				}
				runIDs[ev.RunID] = true
				if ev.Kind == synthusers.LiveRunEnd {
					ends++
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s: only %d of %d run_end events", tc.id, ends, tc.wantRuns)
			}
		}
		cancel()
		if len(runIDs) != tc.wantRuns {
			t.Fatalf("%s: %d live run ids, want %d", tc.id, len(runIDs), tc.wantRuns)
		}

		runs, err := eng.Store().ListRuns(ctx, tc.id, 100)
		if err != nil {
			t.Fatal(err)
		}
		personas := map[string]bool{}
		for _, r := range runs {
			personas[r.Persona] = true
		}
		if len(personas) != tc.wantRuns {
			t.Fatalf("%s: %d distinct personas in saved runs, want %d", tc.id, len(personas), tc.wantRuns)
		}
	}
	if peak < 2 {
		t.Fatalf("visitors never overlapped (peak concurrency %d)", peak)
	}
}
