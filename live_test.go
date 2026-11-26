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
