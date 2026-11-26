// Package explorer implements the free-roaming synthetic user: instead of
// replaying a fixed script it wanders the site like a human would — opens
// the start page, reads for a human "think time", clicks a random link it
// has not seen before, and keeps going until its page budget is spent. Every
// page it loads goes through the engine's checkers, so problems surface
// wherever they happen to be, not just on hand-picked URLs.
package explorer

import (
	"context"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	"synthusers"
	"synthusers/checkers"
)

// Options set engine-wide defaults; per-target values (Target.MaxPages,
// Target.MaxDepth) win over these.
type Options struct {
	// MaxPages is the page budget of one run. Default 20.
	MaxPages int
	// MaxDepth is how many clicks away from the start page it may wander.
	// Default 4.
	MaxDepth int
}

func (o Options) withDefaults() Options {
	if o.MaxPages <= 0 {
		o.MaxPages = 20
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 4
	}
	return o
}

// New builds a synthusers.Runner for the explorer. Wire it with
// engine.SetRunner("explorer", r).
func New(opts Options) *Runner {
	return &Runner{opts: opts.withDefaults()}
}

type Runner struct {
	opts Options
}

type queueItem struct {
	raw   string
	depth int
}

// Run executes one wandering session on the target.
func (r *Runner) Run(ctx context.Context, req *synthusers.RunRequest) ([]synthusers.Finding, synthusers.RunSummary, error) {
	t := req.Target
	base, err := url.Parse(t.StartURL)
	if err != nil || base.Host == "" {
		return nil, synthusers.RunSummary{}, err
	}

	maxPages := t.MaxPages
	if maxPages <= 0 {
		maxPages = r.opts.MaxPages
	}
	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = r.opts.MaxDepth
	}

	visited := map[string]bool{synthusers.NormalizeURLKey(base.String()): true}
	queue := []queueItem{{raw: base.String(), depth: 0}}

	var mu sync.Mutex // queue + visited are touched only here; kept for clarity
	var findings []synthusers.Finding
	summary := synthusers.RunSummary{Persona: req.Persona.Name}
	pages := 0

	for len(queue) > 0 && pages < maxPages {
		if ctx.Err() != nil {
			break
		}
		// Pop a random candidate: human visitors do not walk in BFS order.
		mu.Lock()
		i := rand.Intn(len(queue))
		item := queue[i]
		queue[i] = queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		mu.Unlock()

		page, err := req.Fetch.FetchPage(ctx, item.raw)
		if page == nil {
			if err != nil {
				summary.Error = err.Error()
			}
			break
		}
		if page.Err != nil && page.Status == 0 {
			// Transport failure: nothing to check here; move on to the next
			// queued page instead of burning the budget on dead pages.
			continue
		}
		pages++
		if page.IsHTML() {
			summary.Pages++
			summary.Visited = append(summary.Visited, page.FinalURL)
		}
		findings = append(findings, req.CheckPage(ctx, t, page, req.Fetch)...)

		// Harvest unseen same-host links for later visits.
		if page.IsHTML() && page.Status < 400 && page.Err == nil && item.depth < maxDepth {
			final, perr := url.Parse(page.FinalURL)
			if perr != nil || final == nil {
				final = base
			}
			for _, link := range harvestLinks(page.Body, final, base.Host, visited) {
				mu.Lock()
				visited[link] = false // placeholder: queued
				queue = append(queue, queueItem{raw: link, depth: item.depth + 1})
				mu.Unlock()
			}
		}

		// Human think time before the next click.
		if !req.NoDelays && len(queue) > 0 {
			if err := sleepLikeHuman(ctx, req.Persona); err != nil {
				break
			}
		}
	}

	if pages == 0 && summary.Error == "" && ctx.Err() == nil {
		summary.Error = "explorer could not fetch the start URL"
	}
	return findings, summary, nil
}

// harvestLinks extracts same-host, not-yet-seen links from a page body.
func harvestLinks(body []byte, baseURL *url.URL, host string, visited map[string]bool) []string {
	var fresh []string
	for _, link := range checkers.ExtractLinks(body, baseURL) {
		u, err := url.Parse(link)
		if err != nil {
			continue
		}
		if normalizeHost(u.Host) != normalizeHost(host) {
			continue
		}
		key := synthusers.NormalizeURLKey(link)
		if _, seen := visited[key]; seen {
			continue
		}
		visited[key] = true
		fresh = append(fresh, link)
	}
	return fresh
}

// normalizeHost lowercases and strips a leading "www." so www.example.com
// and example.com are treated as one site.
func normalizeHost(h string) string {
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ".")
	return strings.TrimPrefix(h, "www.")
}

func sleepLikeHuman(ctx context.Context, persona synthusers.Persona) error {
	span := persona.ThinkMax - persona.ThinkMin
	d := persona.ThinkMin
	if span > 0 {
		d = persona.ThinkMin + synthusers.Duration(rand.Int63n(int64(span.Std())))
	}
	t := time.NewTimer(d.Std())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
