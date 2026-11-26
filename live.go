package synthusers

import (
	"fmt"
	"sync"
	"time"
)

// Live event kinds, in the order a run emits them.
const (
	LiveRunStart = "run_start"
	LivePage     = "page"  // an HTML page the user navigated to
	LiveProbe    = "probe" // an asset or link checked from a page
	LiveForm     = "form"  // a form submission
	LiveIssue    = "issue" // a newly opened issue
	LiveRunEnd   = "run_end"
)

// LiveEvent is one thing a synthetic user just did. Subscribers (the
// dashboard) get them as they happen; nothing here is persisted.
type LiveEvent struct {
	Seq      int64     `json:"seq"`
	Kind     string    `json:"kind"`
	RunID    string    `json:"run_id"`
	TargetID string    `json:"target_id"`
	Persona  string    `json:"persona,omitempty"`
	Time     time.Time `json:"time"`

	Mode      string  `json:"mode,omitempty"`   // run_start
	Method    string  `json:"method,omitempty"` // page / probe / form
	URL       string  `json:"url,omitempty"`
	Status    int     `json:"status,omitempty"`
	LatencyMs float64 `json:"latency_ms,omitempty"`
	Err       string  `json:"error,omitempty"`

	Severity  Severity `json:"severity,omitempty"` // issue
	IssueType string   `json:"issue_type,omitempty"`
	Summary   string   `json:"summary,omitempty"`

	Run *Run `json:"run,omitempty"` // run_end
}

const (
	liveBacklog = 1000 // recent events replayed to a new subscriber
	liveBuffer  = 256  // per-subscriber channel depth
)

// liveHub fans events out to subscribers. A slow subscriber drops events
// instead of slowing the synthetic users down.
// shortcut: per-instance only — with sharding, a dashboard sees only its own
// instance's live runs; upgrade to Postgres LISTEN/NOTIFY if that matters.
type liveHub struct {
	mu     sync.Mutex
	seq    int64
	runSeq int64
	recent []LiveEvent
	subs   map[chan LiveEvent]struct{}
}

func (h *liveHub) newRunID(targetID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runSeq++
	return fmt.Sprintf("%s-%d", targetID, h.runSeq)
}

func (h *liveHub) emit(ev LiveEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if len(h.recent) == liveBacklog {
		copy(h.recent, h.recent[1:])
		h.recent = h.recent[:liveBacklog-1]
	}
	h.recent = append(h.recent, ev)
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // subscriber is behind; it catches up via the next refresh
		}
	}
}

// SubscribeLive streams live events. It returns the recent backlog (oldest
// first), a channel of new events, and a cancel func that must be called.
func (e *Engine) SubscribeLive() (backlog []LiveEvent, events <-chan LiveEvent, cancel func()) {
	h := &e.live
	ch := make(chan LiveEvent, liveBuffer)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan LiveEvent]struct{}{}
	}
	h.subs[ch] = struct{}{}
	backlog = append([]LiveEvent(nil), h.recent...)
	h.mu.Unlock()
	return backlog, ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// emitFetch reports one completed request of a run.
func (rf *runFetcher) emitFetch(kind, method, rawURL string, page *FetchedPage) {
	if rf.runID == "" {
		return // fetcher built via NewFetcher outside a run
	}
	ev := LiveEvent{Kind: kind, RunID: rf.runID, TargetID: rf.target.ID, Persona: rf.persona.Name,
		Method: method, URL: rawURL}
	if page != nil {
		ev.Status = page.Status
		ev.LatencyMs = float64(page.Latency.Microseconds()) / 1000
		if page.Err != nil {
			ev.Err = page.Err.Error()
		}
	}
	rf.engine.live.emit(ev)
}
