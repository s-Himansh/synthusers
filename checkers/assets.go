package checkers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"synthusers"
)

// AssetsOptions tune the asset/link checker.
type AssetsOptions struct {
	// MaxAssets caps asset probes per page. Default 25.
	MaxAssets int
	// MaxLinks caps link (a[href]) probes per page. Default 20.
	MaxLinks int
	// ProbeTimeout bounds a single subresource probe. Default 15s.
	ProbeTimeout time.Duration
}

func (o AssetsOptions) withDefaults() AssetsOptions {
	if o.MaxAssets <= 0 {
		o.MaxAssets = 25
	}
	if o.MaxLinks <= 0 {
		o.MaxLinks = 20
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 15 * time.Second
	}
	return o
}

// Assets returns the checker that walks each HTML page's subresources and
// links: broken images/scripts/stylesheets/fonts, unreachable hrefs and
// insecure (mixed) content.
func Assets(opts AssetsOptions) synthusers.Checker { return &assetsChecker{opts: opts.withDefaults()} }

type assetsChecker struct {
	opts AssetsOptions
}

func (c *assetsChecker) Name() string { return "assets" }

func (c *assetsChecker) Check(ctx *synthusers.CheckContext) []Finding {
	p := ctx.Page
	if !p.IsHTML() || p.Status >= 400 || p.Err != nil {
		return nil // error pages are the http checker's job
	}
	base, err := url.Parse(p.FinalURL)
	if base == nil || err != nil {
		base, _ = url.Parse(p.URL)
		if base == nil {
			return nil
		}
	}
	doc, err := parseHTML(p.Body)
	if err != nil {
		return nil
	}
	refs := extractRefs(doc, base)

	var assets, links []Ref
	for _, r := range refs {
		if r.Kind == "link" {
			links = append(links, r)
		} else {
			assets = append(assets, r)
		}
	}

	var out []Finding
	pageHTTPS := base.Scheme == "https"

	// Mixed content needs no network: http:// subresource on an https page.
	var toProbe []Ref
	for _, r := range assets {
		u, err := url.Parse(r.URL)
		if err != nil {
			continue
		}
		if pageHTTPS && u.Scheme == "http" {
			out = append(out, Finding{
				Type: synthusers.TypeMixedContent, Severity: synthusers.SevCritical,
				URL: r.URL, URLKey: synthusers.NormalizeURLKey(r.URL),
				Summary: fmt.Sprintf("mixed content: insecure %s <%s> loaded on https page %s",
					r.Tag, r.URL, base),
				Detail: fmt.Sprintf("tag=%s attr=%s page=%s", r.Tag, r.Attr, p.FinalURL),
			})
			continue
		}
		toProbe = append(toProbe, r)
	}

	if len(toProbe) > c.opts.MaxAssets {
		toProbe = toProbe[:c.opts.MaxAssets]
	}
	seen := map[string]bool{}
	for _, r := range toProbe {
		key := synthusers.NormalizeURLKey(r.URL)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c.probeSubresource(ctx.Ctx, ctx.Fetch, r, base, 0, ctx.ProbeCache)...)
	}

	if len(links) > c.opts.MaxLinks {
		links = links[:c.opts.MaxLinks]
	}
	seenLinks := map[string]bool{}
	for _, r := range links {
		key := synthusers.NormalizeURLKey(r.URL)
		if seenLinks[key] {
			continue
		}
		seenLinks[key] = true
		out = append(out, c.probeLink(ctx.Ctx, ctx.Fetch, r, base, ctx.ProbeCache)...)
	}
	return out
}

// probeSubresource fetches an asset (img/script/css/font/...) and reports
// pass/fail; CSS files get their own url() references probed one level deep.
// Results are cached per run, so a URL referenced from many pages is
// fetched only once.
func (c *assetsChecker) probeSubresource(ctx context.Context, f synthusers.Fetcher, r Ref, page *url.URL, depth int, cache *synthusers.ProbeCache) []Finding {
	key := synthusers.NormalizeURLKey(r.URL)
	var p *synthusers.FetchedPage
	if st, errText, ok := cache.Get(key); ok {
		p = cachedPage(r.URL, st, errText)
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, c.opts.ProbeTimeout)
		var err error
		p, err = f.Fetch(probeCtx, r.URL)
		cancel()
		if p == nil {
			p = &synthusers.FetchedPage{URL: r.URL, Err: err}
		}
		cache.Put(key, p.Status, errString(p.Err))
	}
	sev := synthusers.SevWarning
	if p.Status >= 500 || (p.Status == 0) {
		sev = synthusers.SevCritical
	}

	switch {
	case p.Status == 0:
		return []Finding{{
			Type: synthusers.TypeBrokenAsset, Severity: sev,
			URL: r.URL, URLKey: key,
			Summary: fmt.Sprintf("broken %s: %s unreachable (%s)", r.Tag, r.URL, shortErr(p.Err)),
			Detail:  fmt.Sprintf("tag=%s page=%s error=%q", r.Tag, page, shortErr(p.Err)),
		}}
	case p.Status >= 400:
		return []Finding{{
			Type: synthusers.TypeBrokenAsset, Severity: sev,
			URL: r.URL, URLKey: key,
			Summary: fmt.Sprintf("broken %s: %s returns HTTP %d", r.Tag, r.URL, p.Status),
			Detail:  fmt.Sprintf("tag=%s page=%s status=%d content_type=%s", r.Tag, page, p.Status, p.ContentType()),
		}}
	}

	out := []Finding{{
		Type: synthusers.TypeBrokenAsset, Severity: synthusers.SevInfo,
		URL: r.URL, URLKey: key, Pass: true,
	}}

	// One level of CSS: url()/@import inside stylesheets are assets too.
	if depth == 0 && strings.Contains(p.ContentType(), "text/css") && len(p.Body) > 0 {
		base, _ := url.Parse(p.FinalURL)
		if base == nil {
			base, _ = url.Parse(r.URL)
		}
		if base != nil {
			for i, raw := range cssURLs(p.Body) {
				if i >= 10 {
					break
				}
				u, err := url.Parse(strings.TrimSpace(raw))
				if err != nil || u.Scheme == "" && u.Host == "" && u.Path == "" {
					continue
				}
				if skipSchemes[strings.ToLower(u.Scheme)] {
					continue
				}
				abs := base.ResolveReference(u)
				if abs.Scheme != "http" && abs.Scheme != "https" {
					continue
				}
				abs.Fragment = ""
				out = append(out, c.probeSubresource(ctx, f,
					Ref{URL: abs.String(), Kind: "asset", Tag: "css-url", Attr: "url"},
					base, depth+1, cache)...)
			}
		}
	}
	return out
}

// probeLink checks one a[href] target's availability (cached per run).
func (c *assetsChecker) probeLink(ctx context.Context, f synthusers.Fetcher, r Ref, page *url.URL, cache *synthusers.ProbeCache) []Finding {
	key := synthusers.NormalizeURLKey(r.URL)
	var p *synthusers.FetchedPage
	if st, errText, ok := cache.Get(key); ok {
		p = cachedPage(r.URL, st, errText)
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, c.opts.ProbeTimeout)
		var err error
		p, err = f.Fetch(probeCtx, r.URL)
		cancel()
		if p == nil {
			p = &synthusers.FetchedPage{URL: r.URL, Err: err}
		}
		cache.Put(key, p.Status, errString(p.Err))
	}
	sev := synthusers.SevWarning
	if p.Status >= 500 || p.Status == 0 {
		sev = synthusers.SevCritical
	}

	switch {
	case p.Status == 0:
		return []Finding{{
			Type: synthusers.TypeBrokenLink, Severity: sev,
			URL: r.URL, URLKey: key,
			Summary: fmt.Sprintf("broken link: %s unreachable (%s)", r.URL, shortErr(p.Err)),
			Detail:  fmt.Sprintf("page=%s error=%q", page, shortErr(p.Err)),
		}}
	case p.Status >= 400:
		return []Finding{{
			Type: synthusers.TypeBrokenLink, Severity: sev,
			URL: r.URL, URLKey: key,
			Summary: fmt.Sprintf("broken link: %s returns HTTP %d", r.URL, p.Status),
			Detail:  fmt.Sprintf("page=%s status=%d", page, p.Status),
		}}
	}
	return []Finding{{
		Type: synthusers.TypeBrokenLink, Severity: synthusers.SevInfo,
		URL: r.URL, URLKey: key, Pass: true,
	}}
}

// cachedPage reconstructs a probe result from the per-run cache.
func cachedPage(raw string, status int, errText string) *synthusers.FetchedPage {
	p := &synthusers.FetchedPage{URL: raw, Status: status}
	if errText != "" {
		p.Err = errors.New(errText)
	}
	return p
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
