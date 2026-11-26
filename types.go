// Package synthusers implements an embeddable synthetic-user monitoring
// engine: a fleet of simulated, human-like visitors that continuously browse
// any website it is "attached" to (scripted journeys + a free-roam explorer),
// detect problems while doing so (broken assets and links, HTTP errors,
// slowness, expired TLS certificates, missing content, soft 404s, client-side
// JS errors) and report them as deduplicated, self-resolving issues to a
// pluggable Store (in-memory or Postgres).
//
// The zero-dependency way to use the library:
//
//	eng := synthusers.New(synthusers.Config{}, synthusers.NewMemoryStore())
//	eng.Use(checkers.HTTP(), checkers.Assets(), checkers.Content())
//	eng.UseJS(jserror.New(jserror.Options{})) // optional, needs a browser
//	eng.AddTarget(synthusers.Target{ID: "shop", StartURL: "https://shop.example.com"})
//	eng.Start(ctx)
//
// Run `cmd/synthusersd` for a ready-made daemon that exposes a JSON API and
// a dashboard UI over the same engine.
package synthusers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Severity classifies how badly a finding hurts the site.
type Severity string

const (
	SevCritical Severity = "critical"
	SevWarning  Severity = "warning"
	SevInfo     Severity = "info"
)

// Issue types emitted by the built-in checkers and runners.
const (
	TypeHTTPStatus   = "http_status"   // page unreachable, 4xx/5xx, TLS failure
	TypeSlowResponse = "slow_response" // page latency above threshold
	TypeSSLExpiry    = "ssl_expiry"    // certificate expires soon
	TypeBrokenAsset  = "broken_asset"  // image/script/css/font/... fails to load
	TypeBrokenLink   = "broken_link"   // a[href] leads to 4xx/5xx
	TypeMixedContent = "mixed_content" // http:// subresource on https:// page
	TypeContentFail  = "content_mismatch"
	TypeSoft404      = "soft_404" // 200 OK page that is actually a "not found" screen
	TypeJSError      = "js_error" // uncaught exception / console.error in a real browser
	TypeJourneyFail  = "journey_failed"
)

// Issue lifecycle states.
const (
	StatusOpen     = "open"
	StatusResolved = "resolved"
)

// Target operating modes.
const (
	ModeExplorer = "explorer" // free-roam, human-like crawling
	ModeJourney  = "journey"  // scripted critical flows only
	ModeBoth     = "both"
)

// Duration is a time.Duration that (un)marshals to a human string ("10m",
// "30s") in YAML and JSON, so configs and the REST API stay readable. Plain
// integer values are accepted as nanoseconds for compatibility.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		return d.parse(s)
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*d = Duration(time.Duration(n))
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		return d.parse(s)
	}
	var n int64
	if err := unmarshal(&n); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\" or a number: %w", err)
	}
	*d = Duration(time.Duration(n))
	return nil
}

func (d *Duration) parse(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// JSOptions controls headless-browser checks for a target.
type JSOptions struct {
	// Sample is how many URLs of each run should be loaded in a real
	// browser to catch client-side JS errors. 0 disables the check.
	Sample int `json:"sample" yaml:"sample"`
}

// Assertion is a content check applied to every HTML page a target fetches.
type Assertion struct {
	Contains    string `json:"contains,omitempty" yaml:"contains"`         // body must contain this text
	NotContains string `json:"not_contains,omitempty" yaml:"not_contains"` // body must NOT contain this text
	Regex       string `json:"regex,omitempty" yaml:"regex"`               // body must match this regexp
	MinBytes    int    `json:"min_bytes,omitempty" yaml:"min_bytes"`       // body must be at least this large
}

// FillStep sets the value of a form field before a submit step.
type FillStep struct {
	Selector string `json:"selector" yaml:"selector"`
	Value    string `json:"value" yaml:"value"`
}

// ExpectStep asserts facts about the response produced by the previous step.
type ExpectStep struct {
	Status      int               `json:"status,omitempty" yaml:"status"`
	Contains    string            `json:"contains,omitempty" yaml:"contains"`
	NotContains string            `json:"not_contains,omitempty" yaml:"not_contains"`
	Regex       string            `json:"regex,omitempty" yaml:"regex"`
	Headers     map[string]string `json:"headers,omitempty" yaml:"headers"` // name -> substring
	MaxMS       int               `json:"max_ms,omitempty" yaml:"max_ms"`   // max acceptable duration of the step
}

// Step is one action of a Journey. Exactly one field should be set; a step
// with nothing set acts as a human think-pause.
type Step struct {
	Goto   string      `json:"goto,omitempty" yaml:"goto"`     // relative or absolute URL to open
	Fill   *FillStep   `json:"fill,omitempty" yaml:"fill"`     // fill a form field
	Submit string      `json:"submit,omitempty" yaml:"submit"` // css selector of the form to submit
	Click  string      `json:"click,omitempty" yaml:"click"`   // css selector of link/button to activate
	Expect *ExpectStep `json:"expect,omitempty" yaml:"expect"` // assert on the last response
	Think  Duration    `json:"think,omitempty" yaml:"think"`   // pause like a human
}

// Journey is a scripted critical flow replayed on every run of its target.
type Journey struct {
	Name  string `json:"name" yaml:"name"`
	Steps []Step `json:"steps" yaml:"steps"`
}

// Target is one website (or one section of a site) the synthetic users live
// on. ID must be stable: it is the shard key and the grouping key of issues.
type Target struct {
	ID       string `json:"id" yaml:"id"`
	Name     string `json:"name" yaml:"name"`
	StartURL string `json:"start_url" yaml:"start_url"`
	Mode     string `json:"mode" yaml:"mode"` // explorer | journey | both; default both (or journey-only if no budget configured)
	Enabled  bool   `json:"enabled" yaml:"enabled"`

	Interval Duration `json:"interval,omitempty" yaml:"interval"`   // cadence between runs; 0 = engine default
	MaxPages int      `json:"max_pages,omitempty" yaml:"max_pages"` // explorer budget per run; 0 = engine default
	MaxDepth int      `json:"max_depth,omitempty" yaml:"max_depth"` // explorer link depth; 0 = engine default

	Assertions []Assertion `json:"assertions,omitempty" yaml:"assertions"`
	Journeys   []Journey   `json:"journeys,omitempty" yaml:"journeys"`
	JS         JSOptions   `json:"js,omitempty" yaml:"js"`

	// RateLimit caps requests per second against the site's domain
	// (0 = engine default). This protects the site under test.
	RateLimit float64 `json:"rate_limit,omitempty" yaml:"rate_limit"`

	// Personas rotates visitor identities per run (names from the engine's
	// persona registry, e.g. "chrome-linux", "iphone-safari"). Empty = all.
	Personas []string `json:"personas,omitempty" yaml:"personas"`

	// Visitors is how many synthetic users browse the site at once on each
	// visit, each with a different persona and cookie jar (default 1,
	// capped at the number of personas). They share the site's rate limit.
	Visitors int `json:"visitors,omitempty" yaml:"visitors"`

	// Headers are sent with every request of this target (auth tokens,
	// spam filters' allowlisting, etc).
	Headers map[string]string `json:"headers,omitempty" yaml:"headers"`

	// InsecureSkipVerify accepts the site's TLS certificates as-is (demo
	// sites with self-signed certs). Use only for sites you own.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify"`

	CreatedAt time.Time `json:"created_at,omitempty" yaml:"created_at"`
}

// Mode returns the effective mode of the target.
func (t *Target) ModeOr(defaultMode string) string {
	switch t.Mode {
	case ModeExplorer, ModeJourney, ModeBoth:
		return t.Mode
	default:
		return defaultMode
	}
}

// FetchedPage is the result of one page (or asset) fetch, fed to checkers.
type FetchedPage struct {
	URL         string               // URL as requested
	FinalURL    string               // after redirects ("" = same as URL)
	Status      int                  // 0 when the request failed entirely
	Proto       string               // "HTTP/2.0", "HTTP/1.1", ...
	Header      http.Header          `json:"-"`
	TLS         *tls.ConnectionState `json:"-"` // nil for plain HTTP responses
	Body        []byte               // may be truncated by the fetcher
	Latency     time.Duration
	Redirects   []string // hop chain
	Depth       int      // explorer crawl depth (0 for journeys/assets)
	Persona     string   // persona name used for the request
	FromJourney bool
	Err         error // transport error, if the request never completed
}

// ContentType returns the media type of the response without parameters.
func (p *FetchedPage) ContentType() string {
	if p.Header == nil {
		return ""
	}
	ct := p.Header.Get("Content-Type")
	if i := indexByte(ct, ';'); i >= 0 {
		return ct[:i]
	}
	return ct
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// IsHTML reports whether the response looks like an HTML document.
func (p *FetchedPage) IsHTML() bool {
	ct := p.ContentType()
	return ct == "" || ct == "text/html" || ct == "application/xhtml+xml"
}

// Finding is a single problem (or all-clear signal) a checker or runner
// noticed. Pass=true findings carry no problem: they tell the pipeline that
// the subject was verified healthy, so any open issue with the same scope
// (target + type + URLKey) can be resolved.
type Finding struct {
	Type     string   `json:"type"`
	Severity Severity `json:"severity"`
	URL      string   `json:"url"`     // full URL of the subject, for humans
	URLKey   string   `json:"url_key"` // identity used for grouping/resolution
	Summary  string   `json:"summary"`
	Detail   string   `json:"detail,omitempty"`
	Pass     bool     `json:"pass,omitempty"`
	Source   string   `json:"source,omitempty"` // checker/runner that produced it
}

// Issue is a deduplicated, long-lived problem tracked by a Store.
type Issue struct {
	Fingerprint string     `json:"fingerprint"`
	TargetID    string     `json:"target_id"`
	Type        string     `json:"type"`
	Severity    Severity   `json:"severity"`
	URL         string     `json:"url"`
	URLKey      string     `json:"url_key"`
	Summary     string     `json:"summary"`
	Detail      string     `json:"detail"`
	Status      string     `json:"status"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	Occurrences int64      `json:"occurrences"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// Run is the record of one scheduled visit of a target by the users.
type Run struct {
	ID        int64     `json:"id"`
	TargetID  string    `json:"target_id"`
	Mode      string    `json:"mode"` // explorer | journey | both
	StartedAt time.Time `json:"started_at"`
	Duration  Duration  `json:"duration"`
	Pages     int       `json:"pages"`
	Requests  int       `json:"requests"`
	NewIssues int       `json:"new_issues"`
	Shard     int       `json:"shard"`
	Worker    string    `json:"worker"`
	Persona   string    `json:"persona"`
	Error     string    `json:"error,omitempty"`
}

// IssueFilter narrows ListIssues queries.
type IssueFilter struct {
	Status   string // open | resolved | "" = all
	Severity string
	TargetID string
	Type     string
	Limit    int // default 100
}

// Stats is the dashboard headline data.
type Stats struct {
	Targets      int            `json:"targets"`
	OpenIssues   int            `json:"open_issues"`
	BySeverity   map[string]int `json:"by_severity"`
	ByType       map[string]int `json:"by_type"`
	RunsLast24h  int            `json:"runs_last_24h"`
	PagesLast24h int            `json:"pages_last_24h"`
}

// Store persists targets, runs and issues. Implementations must be safe for
// concurrent use. The engine resolves issues by fingerprint, so fingerprint
// computation lives in this package (see Fingerprint).
type Store interface {
	UpsertTarget(ctx context.Context, t Target) error
	DeleteTarget(ctx context.Context, id string) error
	ListTargets(ctx context.Context) ([]Target, error)

	SaveRun(ctx context.Context, r *Run) error
	ListRuns(ctx context.Context, targetID string, limit int) ([]Run, error)

	// ReportFinding upserts the issue identified by f's fingerprint: a new
	// issue starts open, an existing one bumps last_seen/occurrences and is
	// reopened if it had been resolved. Returns true when the issue was
	// seen for the first time.
	ReportFinding(ctx context.Context, targetID string, f Finding) (isNew bool, err error)

	// ResolveIssues flips the given fingerprints to resolved (only the ones
	// currently open) and returns how many were closed.
	ResolveIssues(ctx context.Context, fingerprints []string) (int, error)

	ListIssues(ctx context.Context, f IssueFilter) ([]Issue, error)
	Stats(ctx context.Context) (Stats, error)

	Close() error
}

// Fingerprint derives the stable issue identity for a scope. Keeping detail
// (status code, error text) out of the hash means a problem's evidence can
// evolve across sightings while the issue stays one row.
func Fingerprint(targetID, issueType, urlKey string) string {
	h := sha256.New()
	h.Write([]byte(targetID))
	h.Write([]byte{0})
	h.Write([]byte(issueType))
	h.Write([]byte{0})
	h.Write([]byte(urlKey))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// FingerprintOf is Fingerprint for a Finding (pass findings resolve the same
// scope they would report).
func FingerprintOf(targetID string, f Finding) string {
	return Fingerprint(targetID, f.Type, f.URLKey)
}

// SeverityRank orders severities for sorting (critical first).
func SeverityRank(s Severity) int {
	switch s {
	case SevCritical:
		return 0
	case SevWarning:
		return 1
	case SevInfo:
		return 2
	default:
		return 3
	}
}

// NormalizeURLKey canonicalizes a URL for use as URLKey: lowercase scheme and
// host, drop fragment, drop default ports, keep path and query. Requests to
// the same resource from different pages therefore share one issue.
func NormalizeURLKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
	}
	return u.String()
}

// Uniq returns the input strings without duplicates, preserving order.
func Uniq(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
