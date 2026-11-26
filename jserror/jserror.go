// Package jserror adds the one check plain HTTP cannot do: it loads pages
// in a real (headless) browser via chromedp and listens for uncaught
// exceptions, console.error calls and browser errors. Because a browser is
// expensive, this checker is optional and sampled: the engine loads only a
// few URLs per run (Target.JS.Sample).
//
// The browser is either launched locally (any Chrome/Chromium on PATH, or
// Options.ExecPath) or reached over the Chrome DevTools Protocol at
// Options.RemoteURL (e.g. a headless-shell sidecar container:
// http://chrome:9222). The first successful check starts one browser that
// all later checks share; when no browser is available, Healthy() is false
// and the engine silently skips JS checks instead of failing runs.
package jserror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"

	"synthusers"
)

// Options configure the browser backend.
type Options struct {
	// RemoteURL is a Chrome DevTools Protocol endpoint of an already
	// running browser, e.g. "http://chrome:9222" or "ws://127.0.0.1:9222".
	RemoteURL string
	// ExecPath overrides local browser discovery.
	ExecPath string
	// Timeout bounds one page load. Default 45s.
	Timeout time.Duration
	// MaxErrors caps findings per page. Default 10.
	MaxErrors int
	// Grace is how long to listen after the load event for async errors.
	// Default 2s.
	Grace time.Duration
	// Logger receives warnings when the browser is unusable.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Second
	}
	if o.MaxErrors <= 0 {
		o.MaxErrors = 10
	}
	if o.Grace <= 0 {
		o.Grace = 2 * time.Second
	}
	return o
}

// Checker is the synthusers.JSRunner implementation.
type Checker struct {
	opts   Options
	logger *slog.Logger

	once       sync.Once
	healthy    bool
	reason     string
	baseCtx    context.Context // context whose first run started the browser
	baseCancel context.CancelFunc
}

// New builds the JS error checker.
func New(opts Options) *Checker {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Checker{opts: opts.withDefaults(), logger: opts.Logger}
}

// Healthy reports whether a browser is usable (checked lazily, once).
func (c *Checker) Healthy() bool {
	c.once.Do(c.probe)
	return c.healthy
}

// Close shuts the shared browser down. Call it on daemon shutdown.
func (c *Checker) Close() {
	c.once.Do(func() { c.healthy = false })
	if c.baseCancel != nil {
		c.baseCancel()
	}
}

func (c *Checker) probe() {
	actx, acancel := c.allocator()
	if actx == nil {
		c.healthy = false
		c.logger.Warn("js checks disabled: " + c.reason)
		return
	}
	ccx, ccancel := chromedp.NewContext(actx)
	if err := chromedp.Do(ccx); err != nil { // starts the browser, opens a tab
		c.healthy = false
		c.reason = "browser probe failed: " + err.Error()
		c.logger.Warn("js checks disabled: " + c.reason)
		ccancel()
		acancel()
		return
	}
	c.healthy = true
	c.baseCtx, c.baseCancel = ccx, func() { ccancel(); acancel() }
	c.logger.Info("js checker ready", "backend", c.backendName())
}

func (c *Checker) backendName() string {
	if c.opts.RemoteURL != "" {
		return "remote " + c.opts.RemoteURL
	}
	return "local " + c.execPath()
}

// allocator returns the browser allocator context, or nil with c.reason set.
func (c *Checker) allocator() (context.Context, context.CancelFunc) {
	if c.opts.RemoteURL != "" {
		return remote.NewAllocator(context.Background(), c.opts.RemoteURL,
			remote.WithDialTimeout(8*time.Second))
	}
	path := c.execPath()
	if path == "" {
		c.reason = "no local Chrome/Chromium found on PATH (set js.exec_path, or deploy a headless-shell sidecar and set js.remote_url)"
		return nil, func() {}
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
	)
	return chromedp.NewExecAllocator(context.Background(), opts...)
}

func (c *Checker) execPath() string {
	if c.opts.ExecPath != "" {
		return c.opts.ExecPath
	}
	for _, name := range []string{
		"google-chrome", "google-chrome-stable", "google-chrome-beta",
		"chromium", "chromium-browser", "chrome", "headless-shell",
	} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// CheckURL loads one page in the browser and collects client-side errors.
func (c *Checker) CheckURL(ctx context.Context, t *synthusers.Target, pageURL string, persona synthusers.Persona) ([]synthusers.Finding, error) {
	if !c.Healthy() {
		return nil, errors.New(c.reason)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	tab, tabCancel := chromedp.NewContext(c.baseCtx) // one tab per check
	defer tabCancel()
	tctx, tcancel := context.WithTimeout(tab, c.opts.Timeout)
	defer tcancel()

	// Subscribe before acting so no message is lost.
	messages := chromedp.Console(tctx)

	err := chromedp.Do(tctx,
		setUserAgent(persona.UserAgent),
		enableDomain(logEnable, cdp.Empty{}),
		enableDomain(networkEnable, network.EnableParams{}),
		chromedp.Navigate(pageURL),
		chromedp.Sleep(c.opts.Grace), // grace window for async errors
	)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", pageURL, err)
	}

	// Close the tab, then drain the buffered console messages.
	tcancel()

	var out []synthusers.Finding
	for msg, merr := range messages {
		if merr != nil {
			if !errors.Is(merr, context.Canceled) {
				return out, merr
			}
			break // normal end: tab closed
		}
		if f, ok := findingFor(msg, pageURL); ok {
			out = append(out, f)
			if len(out) >= c.opts.MaxErrors {
				break
			}
		}
	}
	if len(out) == 0 {
		out = append(out, synthusers.Finding{
			Type: synthusers.TypeJSError, Severity: synthusers.SevInfo,
			URL: pageURL, URLKey: synthusers.NormalizeURLKey(pageURL), Pass: true,
		})
	}
	return out, nil
}
