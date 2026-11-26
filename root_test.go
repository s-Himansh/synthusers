package synthusers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestFingerprintStability(t *testing.T) {
	a := Fingerprint("shop", TypeBrokenAsset, "https://x.test/logo.png")
	b := Fingerprint("shop", TypeBrokenAsset, "https://x.test/logo.png")
	if a != b {
		t.Fatalf("fingerprint not stable: %s != %s", a, b)
	}
	if a == Fingerprint("shop2", TypeBrokenAsset, "https://x.test/logo.png") {
		t.Fatal("fingerprint must differ per target")
	}
	if a == Fingerprint("shop", TypeBrokenLink, "https://x.test/logo.png") {
		t.Fatal("fingerprint must differ per type")
	}
	if a == Fingerprint("shop", TypeBrokenAsset, "https://x.test/other.png") {
		t.Fatal("fingerprint must differ per url key")
	}
}

func TestNormalizeURLKey(t *testing.T) {
	cases := map[string]string{
		"https://Example.com/logo.png": "https://example.com/logo.png",
		"https://example.com:443/a":    "https://example.com/a",
		"http://example.com:80/a?b=1":  "http://example.com/a?b=1",
		"https://example.com/page#top": "https://example.com/page",
		"https://example.com:8443/a":   "https://example.com:8443/a",
	}
	for in, want := range cases {
		if got := NormalizeURLKey(in); got != want {
			t.Errorf("NormalizeURLKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	f := Finding{Type: TypeBrokenAsset, Severity: SevWarning, URL: "https://x.test/logo.png", Summary: "broken logo"}

	isNew, err := s.ReportFinding(ctx, "shop", f)
	if err != nil || !isNew {
		t.Fatalf("first report: isNew=%v err=%v", isNew, err)
	}
	iss, err := s.ListIssues(ctx, IssueFilter{Status: StatusOpen})
	if err != nil || len(iss) != 1 {
		t.Fatalf("want 1 open issue, got %d (err %v)", len(iss), err)
	}
	first := iss[0]
	if first.Occurrences != 1 {
		t.Fatalf("occurrences = %d, want 1", first.Occurrences)
	}

	// Second sighting: same fingerprint, occurrence bumps, not new.
	time.Sleep(5 * time.Millisecond)
	isNew, err = s.ReportFinding(ctx, "shop", f)
	if err != nil || isNew {
		t.Fatalf("second report: isNew=%v err=%v", isNew, err)
	}
	iss, _ = s.ListIssues(ctx, IssueFilter{})
	if iss[0].Occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2", iss[0].Occurrences)
	}
	if !iss[0].LastSeen.After(iss[0].FirstSeen) {
		t.Fatal("last_seen must advance")
	}

	// Resolve.
	fp := FingerprintOf("shop", f)
	n, err := s.ResolveIssues(ctx, []string{fp})
	if err != nil || n != 1 {
		t.Fatalf("resolve: n=%d err=%v", n, err)
	}
	iss, _ = s.ListIssues(ctx, IssueFilter{Status: StatusResolved})
	if len(iss) != 1 || iss[0].ResolvedAt == nil {
		t.Fatalf("want 1 resolved issue with resolved_at, got %d", len(iss))
	}

	// Re-report reopens.
	isNew, _ = s.ReportFinding(ctx, "shop", f)
	if isNew {
		t.Fatal("re-report after resolve must not be new")
	}
	iss, _ = s.ListIssues(ctx, IssueFilter{Status: StatusOpen})
	if len(iss) != 1 {
		t.Fatalf("issue must reopen, got %d open", len(iss))
	}
}

func TestTargetJSONRoundTrip(t *testing.T) {
	in := Target{
		ID:       "shop",
		StartURL: "https://x.test/",
		Interval: Duration(10 * time.Minute),
		Journeys: []Journey{{Name: "login", Steps: []Step{
			{Goto: "/login"},
			{Fill: &FillStep{Selector: "input[name=u]", Value: "demo"}},
			{Expect: &ExpectStep{Status: 200, Contains: "hello"}},
		}}},
		JS: JSOptions{Sample: 2},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Target
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Interval.Std() != 10*time.Minute {
		t.Fatalf("interval = %v", out.Interval)
	}
	if len(out.Journeys) != 1 || out.Journeys[0].Steps[1].Fill.Value != "demo" {
		t.Fatalf("journeys lost: %+v", out.Journeys)
	}
	if out.JS.Sample != 2 {
		t.Fatalf("js sample lost")
	}
}

func TestShardOf(t *testing.T) {
	if shardOf("a", 1) != 0 {
		t.Fatal("single shard always 0")
	}
	seen := map[int]bool{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		s := shardOf(id, 3)
		if s < 0 || s >= 3 {
			t.Fatalf("shard out of range: %d", s)
		}
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Fatal("shards should distribute ids")
	}
}

func TestEngineShardingFiltersTargets(t *testing.T) {
	eng := New(Config{ShardIndex: 0, ShardCount: 2, DisableDelays: true}, NewMemoryStore())
	if eng.Config().ShardCount != 2 {
		t.Fatal("shard config lost")
	}
	ids := []string{}
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		if shardOf(id, 2) == 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) == 4 {
		t.Fatalf("sharding must split ids, got %v", ids)
	}
}

func TestRateLimiterSlots(t *testing.T) {
	rl := newRateLimiter(1000) // very fast; should not throttle
	for i := 0; i < 50; i++ {
		if err := rl.acquire(context.Background(), "x.test", 0); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	// One request per second: second acquire must wait.
	rl2 := newRateLimiter(1)
	start := time.Now()
	_ = rl2.acquire(context.Background(), "y.test", 0)
	_ = rl2.acquire(context.Background(), "y.test", 0)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("second acquire should be rate limited to ~1s, took %s", elapsed)
	}
}

func TestDurationYAMLAndJSON(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`10m`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Std() != 10*time.Minute {
		t.Fatalf("yaml duration = %v", d)
	}
	if err := yaml.Unmarshal([]byte(`"90s"`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Std() != 90*time.Second {
		t.Fatalf("yaml quoted duration = %v", d)
	}
	if err := yaml.Unmarshal([]byte(`nope`), &d); err == nil {
		t.Fatal("invalid duration must error")
	}
	if !strings.Contains(d.String(), "90s") && d.String() != "1m30s" {
		t.Fatalf("unexpected string %q", d.String())
	}
}
