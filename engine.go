package synthusers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Checker inspects one fetched page and reports findings (problems or
// all-clear signals). Implementations live in the checkers subpackage; wire
// them with Engine.Use.
type Checker interface {
	Name() string
	Check(c *CheckContext) []Finding
}

// CheckContext carries everything a checker needs: the page just fetched,
// its target, a context for probes, and a Fetcher for subresource checks
// (broken assets, link checks). ProbeCache deduplicates subresource probes
// across one run so a URL linked from many pages is fetched only once.
type CheckContext struct {
	Ctx        context.Context
	Target     *Target
	Page       *FetchedPage
	Fetch      Fetcher
	ProbeCache *ProbeCache
}

// ProbeCache caches subresource probe results for one run.
type ProbeCache struct {
	mu     sync.Mutex
	status map[string]int
	errs   map[string]string
}

func NewProbeCache() *ProbeCache {
	return &ProbeCache{status: map[string]int{}, errs: map[string]string{}}
}

// Get returns the cached result of a prior probe of urlKey.
func (pc *ProbeCache) Get(urlKey string) (status int, errText string, ok bool) {
	if pc == nil {
		return 0, "", false
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	st, ok := pc.status[urlKey]
	if !ok {
		return 0, "", false
	}
	return st, pc.errs[urlKey], true
}

// Put records a probe result.
func (pc *ProbeCache) Put(urlKey string, status int, errText string) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.status[urlKey] = status
	pc.errs[urlKey] = errText
}

// CheckPage runs all registered checkers over one fetched page and returns
// their combined findings. Runners call this for every page they load.
func (e *Engine) CheckPage(ctx context.Context, target *Target, page *FetchedPage, fetch Fetcher) []Finding {
	var out []Finding
	for _, c := range e.checkers {
		cc := &CheckContext{Ctx: ctx, Target: target, Page: page, Fetch: fetch}
		if rf, ok := fetch.(*runFetcher); ok {
			cc.ProbeCache = rf.probeCache
		}
		findings := c.Check(cc)
		for _, f := range findings {
			f.Source = c.Name()
		}
		out = append(out, findings...)
	}
	return out
}

// Fetcher fetches a URL under the engine's rate limits and returns a
// (possibly truncated) response. Used by checkers to probe subresources.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*FetchedPage, error)
}

// Runner executes one scheduled visit of a target: the explorer crawler
// (explorer package) and the journey player (journey package) implement it.
// The engine does not import them; wiring is done by the caller (daemon,
// tests) so the core stays dependency-free.
type Runner interface {
	Run(ctx context.Context, r *RunRequest) ([]Finding, RunSummary, error)
}

// RunRequest is what a runner gets for one visit.
type RunRequest struct {
	Target   *Target
	Persona  Persona
	Fetch    PageFetcher // rate-limited fetcher sharing the persona's cookies
	NoDelays bool        // true in tests: skip human think-time pauses
	Logger   *slog.Logger
	// CheckPage runs the registered checkers over a fetched page. Runners
	// call it after every page load; its findings join the run's findings.
	CheckPage func(ctx context.Context, target *Target, page *FetchedPage, fetch Fetcher) []Finding
}

// RunSummary describes what a visit did.
type RunSummary struct {
	Pages    int // HTML pages loaded
	Requests int // total HTTP requests made (pages + assets + links)
	Persona  string
	Visited  []string // every page URL reached (HTML only), for JS sampling
	Error    string   // fatal run error, "" on success
}

// PageFetcher is the page-level fetch surface runners use: full bodies,
// per-run cookie jar, persona headers.
type PageFetcher interface {
	FetchPage(ctx context.Context, rawURL string) (*FetchedPage, error)
	Fetch(ctx context.Context, rawURL string) (*FetchedPage, error) // probes: small bodies
	Client() *http.Client
	// SubmitForm sends a form the way a browser does: GET moves the values
	// into the query string, POST sends an urlencoded body. Redirect
	// chains are followed and cookies (logins) are kept.
	SubmitForm(ctx context.Context, action, method string, values url.Values) (*FetchedPage, error)
}

// JSRunner loads pages in a real browser to catch client-side errors
// (implemented by the jserror package via chromedp). Optional.
type JSRunner interface {
	Healthy() bool
	CheckURL(ctx context.Context, t *Target, pageURL string, persona Persona) ([]Finding, error)
}

// Config tunes the engine. Zero values pick production-safe defaults.
type Config struct {
	Interval      time.Duration // default cadence between runs of a target (5m)
	Workers       int           // concurrent target runs (8)
	ShardIndex    int           // this instance's shard (0-based)
	ShardCount    int           // total shards across the fleet (1 = no sharding)
	RateLimit     float64       // requests/sec per target domain (2)
	Timeout       time.Duration // per-request timeout (30s)
	RunTimeout    time.Duration // wall-clock budget per run (15m)
	MaxBodyBytes  int64         // max page body kept for checks (4 MiB)
	DisableDelays bool          // tests only: drop human think-time pauses
	Personas      []Persona     // extra personas added to the built-in crowd
	Logger        *slog.Logger
	Now           func() time.Time
}

func (c *Config) fillDefaults() {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.ShardCount <= 0 {
		c.ShardCount = 1
	}
	if c.ShardIndex < 0 || c.ShardIndex >= c.ShardCount {
		c.ShardIndex = 0
	}
	if c.RateLimit <= 0 {
		c.RateLimit = 2
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = 15 * time.Minute
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 4 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Engine is the synthetic-user fleet manager: it schedules target visits,
// executes them through runners, feeds the results through checkers and
// persists issues and runs in its Store.
type Engine struct {
	cfg      Config
	store    Store
	logger   *slog.Logger
	limiter  *rateLimiter
	personas *personaPool

	client *http.Client // shared base client (no jar; per-run copies add one)
	baseRT *http.Transport

	checkers []Checker
	jsRunner JSRunner

	explorerRunner Runner
	journeyRunner  Runner

	mu       sync.Mutex
	lastRun  map[string]time.Time
	inFlight map[string]bool
	jobs     chan job
	started  bool

	live liveHub // in-memory fan-out of what runs are doing right now
}

type job struct {
	targetID string
	force    bool
}

// New builds an Engine writing to the given store. Register checkers and
// runners before Start (see Engine.Use, Engine.SetRunner).
func New(cfg Config, store Store) *Engine {
	cfg.fillDefaults()
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:               http.ProxyFromEnvironment,
	}
	limiter := newRateLimiter(cfg.RateLimit)
	client := &http.Client{
		Transport: &limitingTransport{base: tr, limiter: limiter},
		Timeout:   cfg.Timeout,
	}
	return &Engine{
		cfg:      cfg,
		store:    store,
		logger:   cfg.Logger,
		limiter:  limiter,
		personas: newPersonaPool(cfg.Personas),
		client:   client,
		baseRT:   tr,
		lastRun:  map[string]time.Time{},
		inFlight: map[string]bool{},
		jobs:     make(chan job, 512),
	}
}

// Use registers a page checker.
func (e *Engine) Use(c Checker) *Engine {
	e.checkers = append(e.checkers, c)
	return e
}

// UseJS registers the optional real-browser checker (jserror package).
func (e *Engine) UseJS(r JSRunner) *Engine {
	e.jsRunner = r
	return e
}

// SetRunner wires a visit runner ("explorer" for the crawler, "journey" for
// scripted flows).
func (e *Engine) SetRunner(kind string, r Runner) *Engine {
	switch kind {
	case ModeExplorer:
		e.explorerRunner = r
	case ModeJourney:
		e.journeyRunner = r
	}
	return e
}

// Store exposes the underlying store (the dashboard reads through it).
func (e *Engine) Store() Store { return e.store }

// Config returns the effective configuration.
func (e *Engine) Config() Config { return e.cfg }

// Personas returns the persona registry.
func (e *Engine) Personas() []Persona { return e.personas.list() }

// AddTarget registers a website with the fleet (persisted in the store and
// scheduled immediately).
func (e *Engine) AddTarget(t Target) error {
	if t.ID == "" {
		if t.Name == "" {
			return errors.New("target needs an id or a name")
		}
		t.ID = slugify(t.Name)
	}
	if _, err := url.Parse(t.StartURL); err != nil || t.StartURL == "" {
		return fmt.Errorf("target %q: invalid start_url %q", t.ID, t.StartURL)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = e.cfg.Now()
	}
	return e.store.UpsertTarget(context.Background(), t)
}

// RemoveTarget stops watching a site.
func (e *Engine) RemoveTarget(id string) error {
	return e.store.DeleteTarget(context.Background(), id)
}

// Targets lists the watched sites.
func (e *Engine) Targets() ([]Target, error) { return e.store.ListTargets(context.Background()) }

// TriggerRun schedules an immediate (asynchronous) run of a target.
func (e *Engine) TriggerRun(targetID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inFlight[targetID] {
		return fmt.Errorf("target %q already running", targetID)
	}
	select {
	case e.jobs <- job{targetID: targetID, force: true}:
		return nil
	default:
		return fmt.Errorf("run queue is full")
	}
}

// Start launches the scheduler and the worker pool. It returns immediately;
// cancel the context to stop the fleet.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	// seed lastRun with jittered future times so a fleet of targets does
	// not all fire at startup (and restarts spread the load).
	now := e.cfg.Now()
	targets, _ := e.store.ListTargets(ctx)
	for _, t := range targets {
		if e.cfg.ShardCount > 1 && shardOf(t.ID, e.cfg.ShardCount) != e.cfg.ShardIndex {
			continue
		}
		jitter := time.Duration(fnv32a(t.ID)%uint32(e.cfg.Interval.Seconds())) * time.Second
		e.lastRun[t.ID] = now.Add(jitter - e.cfg.Interval)
	}

	for i := 0; i < e.cfg.Workers; i++ {
		go e.worker(ctx, i)
	}
	go e.scheduleLoop(ctx)
	e.logger.Info("engine started",
		"workers", e.cfg.Workers,
		"shard", fmt.Sprintf("%d/%d", e.cfg.ShardIndex, e.cfg.ShardCount),
		"interval", e.cfg.Interval,
		"targets", len(targets))
}

func (e *Engine) scheduleLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.scheduleDue(ctx)
		}
	}
}

func (e *Engine) scheduleDue(ctx context.Context) {
	targets, err := e.store.ListTargets(ctx)
	if err != nil {
		e.logger.Error("listing targets", "err", err)
		return
	}
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		if e.cfg.ShardCount > 1 && shardOf(t.ID, e.cfg.ShardCount) != e.cfg.ShardIndex {
			continue
		}
		if e.inFlight[t.ID] {
			continue
		}
		interval := t.Interval.Std()
		if interval <= 0 {
			interval = e.cfg.Interval
		}
		last, ok := e.lastRun[t.ID]
		if ok && now.Sub(last) < interval {
			continue
		}
		select {
		case e.jobs <- job{targetID: t.ID}:
		default:
			return // queue full; try again next tick
		}
	}
}

func (e *Engine) worker(ctx context.Context, id int) {
	name := fmt.Sprintf("worker-%d", id)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.jobs:
			targets, err := e.store.ListTargets(ctx)
			if err != nil {
				e.logger.Error("listing targets", "err", err)
				continue
			}
			var t *Target
			for i := range targets {
				if targets[i].ID == j.targetID {
					t = &targets[i]
					break
				}
			}
			if t == nil || !t.Enabled {
				continue
			}
			e.mu.Lock()
			if e.inFlight[t.ID] {
				e.mu.Unlock()
				continue
			}
			e.inFlight[t.ID] = true
			e.lastRun[t.ID] = e.cfg.Now()
			e.mu.Unlock()

			runCtx, cancel := context.WithTimeout(ctx, e.cfg.RunTimeout)
			runs := e.runTarget(runCtx, t, name, j.force)
			cancel()

			for i := range runs {
				if err := e.store.SaveRun(context.Background(), &runs[i]); err != nil {
					e.logger.Error("saving run", "err", err)
				}
			}

			e.mu.Lock()
			delete(e.inFlight, t.ID)
			e.mu.Unlock()
		}
	}
}

// runTarget sends the target's visitors (Target.Visitors, distinct
// personas) to the site at once and returns one Run per visitor.
func (e *Engine) runTarget(ctx context.Context, t *Target, workerName string, force bool) []Run {
	personas := e.personas.pickN(t, t.Visitors)
	runs := make([]Run, len(personas))
	var wg sync.WaitGroup
	for i, p := range personas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Only the first visitor samples pages in Chrome: a crowd should
			// not multiply browser load.
			runs[i] = e.runVisit(ctx, t, p, workerName, i == 0)
		}()
	}
	wg.Wait()
	return runs
}

// runVisit performs one visitor's full visit: explorer crawl and/or
// journeys, plus optional JS-error sampling, feeding every finding through
// the issue pipeline.
func (e *Engine) runVisit(ctx context.Context, t *Target, persona Persona, workerName string, sampleJS bool) Run {
	mode := t.ModeOr(ModeBoth)
	started := e.cfg.Now()
	fetcher := e.fetcherFor(t, persona)
	fetcher.runID = e.live.newRunID(t.ID)
	e.live.emit(LiveEvent{Kind: LiveRunStart, RunID: fetcher.runID, TargetID: t.ID, Persona: persona.Name, Mode: mode, URL: t.StartURL})

	req := &RunRequest{
		Target:    t,
		Persona:   persona,
		Fetch:     fetcher,
		NoDelays:  e.cfg.DisableDelays,
		Logger:    e.logger,
		CheckPage: e.CheckPage,
	}

	var findings []Finding
	summary := RunSummary{Persona: persona.Name}
	var firstErr error

	if mode == ModeExplorer || mode == ModeBoth {
		if r := e.explorerRunner; r != nil {
			f, s, err := r.Run(ctx, req)
			findings = append(findings, f...)
			summary.Pages += s.Pages
			summary.Requests += s.Requests
			summary.Visited = append(summary.Visited, s.Visited...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if mode == ModeJourney || mode == ModeBoth {
		if r := e.journeyRunner; r != nil {
			f, s, err := r.Run(ctx, req)
			findings = append(findings, f...)
			summary.Pages += s.Pages
			summary.Requests += s.Requests
			summary.Visited = append(summary.Visited, s.Visited...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	// Sample a few of the visited pages in a real browser for JS errors.
	newIssues := e.pipeline(ctx, t, fetcher.runID, findings)
	if sampleJS && e.jsRunner != nil && e.jsRunner.Healthy() && t.JS.Sample > 0 {
		for _, u := range sampleURLs(summary.Visited, t.JS.Sample) {
			jsFindings, err := e.jsRunner.CheckURL(ctx, t, u, persona)
			if err != nil {
				e.logger.Warn("js check failed", "url", u, "err", err)
				continue
			}
			newIssues += e.pipeline(ctx, t, fetcher.runID, jsFindings)
		}
	}

	run := Run{
		TargetID:  t.ID,
		Mode:      mode,
		StartedAt: started,
		Duration:  Duration(e.cfg.Now().Sub(started)),
		Pages:     summary.Pages,
		Requests:  int(fetcher.requests.Load()),
		NewIssues: newIssues,
		Shard:     e.cfg.ShardIndex,
		Worker:    workerName,
		Persona:   persona.Name,
	}
	if firstErr != nil {
		run.Error = firstErr.Error()
	}
	ended := run
	e.live.emit(LiveEvent{Kind: LiveRunEnd, RunID: fetcher.runID, TargetID: t.ID, Persona: persona.Name, Run: &ended})
	return run
}

// pipeline persists findings and resolves scopes that passed. Returns the
// number of newly created issues.
func (e *Engine) pipeline(ctx context.Context, t *Target, runID string, findings []Finding) int {
	var toResolve []string
	newIssues := 0
	for _, f := range findings {
		if f.URLKey == "" {
			f.URLKey = f.URL
		}
		if f.Pass {
			toResolve = append(toResolve, FingerprintOf(t.ID, f))
			continue
		}
		isNew, err := e.store.ReportFinding(ctx, t.ID, f)
		if err != nil {
			e.logger.Error("reporting finding", "err", err)
			continue
		}
		if isNew {
			newIssues++
			e.live.emit(LiveEvent{Kind: LiveIssue, RunID: runID, TargetID: t.ID, URL: f.URL,
				Severity: f.Severity, IssueType: f.Type, Summary: f.Summary})
			e.logger.Info("new issue",
				"target", t.ID, "type", f.Type, "severity", f.Severity,
				"url", f.URL, "summary", f.Summary)
		}
	}
	if len(toResolve) > 0 {
		if _, err := e.store.ResolveIssues(ctx, toResolve); err != nil {
			e.logger.Error("resolving issues", "err", err)
		}
	}
	return newIssues
}

// RunOnce performs a synchronous run of one target now (used by tests and
// the dashboard's "run now" button).
// Every visitor's run is saved; the first visitor's run is returned.
func (e *Engine) RunOnce(ctx context.Context, targetID string) (Run, error) {
	targets, err := e.store.ListTargets(ctx)
	if err != nil {
		return Run{}, err
	}
	var t *Target
	for i := range targets {
		if targets[i].ID == targetID {
			t = &targets[i]
			break
		}
	}
	if t == nil {
		return Run{}, fmt.Errorf("unknown target %q", targetID)
	}
	if !t.Enabled {
		return Run{}, fmt.Errorf("target %q is disabled", targetID)
	}
	runs := e.runTarget(ctx, t, "manual", true)
	for i := range runs {
		if err := e.store.SaveRun(ctx, &runs[i]); err != nil {
			return runs[0], err
		}
	}
	return runs[0], nil
}

// Close releases the store.
func (e *Engine) Close() error { return e.store.Close() }

// ---- fetch plumbing -----------------------------------------------------

// fetcherFor builds the per-run fetch surface: the shared rate-limited
// transport, the persona's cookie jar and headers, and the target's TLS
// policy.
func (e *Engine) fetcherFor(t *Target, persona Persona) *runFetcher {
	return &runFetcher{engine: e, target: t, persona: persona}
}

// NewFetcher returns a fetch surface for custom runners: rate-limited by
// the engine, keyed to the persona's cookie jar (so sessions persist) and
// honoring the target's TLS policy. Useful when embedding the library with
// your own Runner implementations.
func (e *Engine) NewFetcher(t *Target, persona Persona) PageFetcher {
	return e.fetcherFor(t, persona)
}

type runFetcher struct {
	engine     *Engine
	target     *Target
	persona    Persona
	once       sync.Once
	client     *http.Client
	requests   atomic.Int64 // every outbound request, pages and probes alike
	probeCache *ProbeCache  // per-run subresource probe results
	runID      string       // live-event run id; "" outside engine runs
}

// RequestCount reports how many HTTP requests this run's fetcher made.
func (rf *runFetcher) RequestCount() int64 { return rf.requests.Load() }

func (rf *runFetcher) Client() *http.Client {
	rf.init()
	return rf.client
}

func (rf *runFetcher) init() {
	rf.once.Do(func() {
		if rf.probeCache == nil {
			rf.probeCache = NewProbeCache()
		}
		jar := rf.engine.personas.jar(rf.persona.Name, rf.target.ID)
		c := *rf.engine.client // shallow copy: shares the safe transport
		c.Jar = jar
		if rf.target.InsecureSkipVerify {
			tr := rf.engine.baseRT.Clone()
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
			c.Transport = &limitingTransport{base: tr, limiter: rf.engine.limiter}
		}
		rf.client = &c
	})
}

func (rf *runFetcher) do(ctx context.Context, kind, rawURL string, maxBytes int64) (*FetchedPage, error) {
	rf.init()
	rf.requests.Add(1)
	page, err := rf.engine.doFetch(ctx, rf.client, rf.target, rf.persona, http.MethodGet, rawURL, nil, "", maxBytes)
	if err == nil {
		rf.emitFetch(kind, http.MethodGet, rawURL, page)
	}
	return page, err
}

// SubmitForm implements PageFetcher.SubmitForm.
func (rf *runFetcher) SubmitForm(ctx context.Context, action, method string, values url.Values) (*FetchedPage, error) {
	rf.init()
	method = strings.ToUpper(method)
	if method == "" {
		method = http.MethodGet
	}
	rf.requests.Add(1)
	switch method {
	case http.MethodGet:
		u, err := url.Parse(action)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		for k, vs := range values {
			for _, v := range vs {
				q.Set(k, v) // form fields are unique; last value wins
			}
		}
		u.RawQuery = q.Encode()
		return rf.formDone(method, action)(rf.engine.doFetch(ctx, rf.client, rf.target, rf.persona, http.MethodGet, u.String(), nil, "", rf.engine.cfg.MaxBodyBytes))
	default:
		body := strings.NewReader(values.Encode())
		return rf.formDone(method, action)(rf.engine.doFetch(ctx, rf.client, rf.target, rf.persona, method, action, body, "application/x-www-form-urlencoded", rf.engine.cfg.MaxBodyBytes))
	}
}

// formDone reports a finished form submission as a live event.
func (rf *runFetcher) formDone(method, action string) func(*FetchedPage, error) (*FetchedPage, error) {
	return func(page *FetchedPage, err error) (*FetchedPage, error) {
		if err == nil {
			rf.emitFetch(LiveForm, method, action, page)
		}
		return page, err
	}
}

func (rf *runFetcher) FetchPage(ctx context.Context, rawURL string) (*FetchedPage, error) {
	return rf.do(ctx, LivePage, rawURL, rf.engine.cfg.MaxBodyBytes)
}

func (rf *runFetcher) Fetch(ctx context.Context, rawURL string) (*FetchedPage, error) {
	const probeBytes = 512 << 10
	return rf.do(ctx, LiveProbe, rawURL, probeBytes)
}

// fetch executes one GET with timing, redirect tracking and body limits.
func (e *Engine) fetch(ctx context.Context, client *http.Client, t *Target, persona Persona, rawURL string, maxBytes int64) (*FetchedPage, error) {
	return e.doFetch(ctx, client, t, persona, http.MethodGet, rawURL, nil, "", maxBytes)
}

// doFetch executes one request with timing, redirect tracking and body
// limits. Transport errors are returned as data (page.Err), not Go errors.
func (e *Engine) doFetch(ctx context.Context, client *http.Client, t *Target, persona Persona, method, rawURL string, body io.Reader, contentType string, maxBytes int64) (*FetchedPage, error) {
	reqCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	setPersonaHeaders(req, persona, t)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Track redirect hops for checker diagnostics.
	var hops []string
	c := *client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		hops = append(hops, via[len(via)-1].URL.String())
		if len(via) > 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}

	start := e.cfg.Now()
	resp, err := c.Do(req)
	latency := e.cfg.Now().Sub(start)
	page := &FetchedPage{URL: rawURL, Latency: latency, Persona: persona.Name}
	if err != nil {
		// Transport errors are data for checkers, not runner errors.
		page.Err = err
		page.Redirects = hops
		return page, nil
	}
	defer resp.Body.Close()
	page.Status = resp.StatusCode
	page.Proto = resp.Proto
	page.Header = resp.Header
	page.TLS = resp.TLS
	page.FinalURL = resp.Request.URL.String()
	page.Redirects = hops
	pageBody, readErr := ioLimit(resp.Body, maxBytes)
	page.Body = pageBody
	if readErr != nil && page.Err == nil {
		page.Err = readErr
	}
	return page, nil
}

func setPersonaHeaders(req *http.Request, persona Persona, t *Target) {
	if persona.UserAgent != "" {
		req.Header.Set("User-Agent", persona.UserAgent)
	}
	if persona.AcceptLanguage != "" {
		req.Header.Set("Accept-Language", persona.AcceptLanguage)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	for k, v := range persona.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
}

// ioLimit reads up to max bytes from r.
func ioLimit(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 4 << 20
	}
	buf, err := io.ReadAll(io.LimitReader(r, max))
	if err != nil && len(buf) > 0 {
		return buf, nil // partial body is still useful
	}
	return buf, err
}

// sampleURLs picks up to n URLs pseudo-randomly from the visited list.
func sampleURLs(visited []string, n int) []string {
	if n <= 0 || len(visited) == 0 {
		return nil
	}
	visited = Uniq(visited)
	if len(visited) <= n {
		return visited
	}
	rand.Shuffle(len(visited), func(i, j int) { visited[i], visited[j] = visited[j], visited[i] })
	return visited[:n]
}

// shardOf maps a target id to its shard.
func shardOf(id string, shards int) int {
	if shards <= 1 {
		return 0
	}
	return int(fnv32a(id) % uint32(shards))
}

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// slugify turns a name into a stable id slug.
func slugify(s string) string {
	var b []byte
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, byte(r))
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b = append(b, '-')
		}
	}
	out := strings.Trim(string(b), "-")
	if out == "" {
		out = fmt.Sprintf("target-%d", time.Now().UnixNano()%100000)
	}
	return out
}
