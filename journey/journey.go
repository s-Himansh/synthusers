// Package journey replays scripted critical flows — login, checkout,
// search, signup — as HTTP a real browser would produce: pages are fetched
// with the persona's cookies and headers, forms are filled and submitted,
// every response can be asserted. A failed step becomes a journey_failed
// issue; a fully passing journey resolves it again.
package journey

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"synthusers"
)

// New builds a synthusers.Runner for journey replay. Wire it with
// engine.SetRunner("journey", r).
func New() *Runner { return &Runner{} }

type Runner struct{}

func (r *Runner) Name() string { return "journey" }

// Run executes every journey configured on the target.
func (r *Runner) Run(ctx context.Context, req *synthusers.RunRequest) ([]synthusers.Finding, synthusers.RunSummary, error) {
	var findings []synthusers.Finding
	summary := synthusers.RunSummary{Persona: req.Persona.Name}
	for _, j := range req.Target.Journeys {
		if ctx.Err() != nil {
			break
		}
		f, visited, err := r.runOne(ctx, req, j)
		findings = append(findings, f...)
		summary.Visited = append(summary.Visited, visited...)
		if err != nil {
			summary.Error = fmt.Sprintf("journey %q: %v", j.Name, err)
			return findings, summary, nil // run error recorded, not fatal
		}
		if !req.NoDelays && req.Persona.ThinkMin > 0 {
			_ = sleepCtx(ctx, req.Persona.ThinkMin.Std())
		}
	}
	summary.Pages = len(summary.Visited)
	return findings, summary, nil
}

// runOne replays a single journey and returns its findings.
func (r *Runner) runOne(ctx context.Context, req *synthusers.RunRequest, j synthusers.Journey) ([]synthusers.Finding, []string, error) {
	t := req.Target
	base, err := url.Parse(t.StartURL)
	if err != nil || base.Host == "" {
		return nil, nil, fmt.Errorf("invalid start_url %q: %w", t.StartURL, err)
	}
	scope := "journey:" + j.Name

	fail := func(step int, why string) ([]synthusers.Finding, []string, error) {
		return []synthusers.Finding{{
			Type:     synthusers.TypeJourneyFail,
			Severity: synthusers.SevCritical,
			URL:      t.StartURL,
			URLKey:   scope,
			Summary:  fmt.Sprintf("journey %q failed at step %d: %s", j.Name, step+1, why),
			Detail:   describeJourney(j, step),
		}}, nil, nil
	}

	var lastPage *synthusers.FetchedPage
	var visited []string
	filled := map[string]string{} // journey-filled field values
	current := base.String()

	for i, step := range j.Steps {
		if ctx.Err() != nil {
			return nil, visited, ctx.Err()
		}
		switch {
		case step.Goto != "":
			target := resolveURL(base, current, step.Goto)
			page, perr := req.Fetch.FetchPage(ctx, target)
			if perr != nil {
				return fail(i, "goto "+step.Goto+": "+perr.Error())
			}
			if page == nil {
				return fail(i, "goto "+step.Goto+": no response")
			}
			lastPage = page
			current = pageOrReqURL(page)
			if page.IsHTML() {
				visited = append(visited, page.FinalURL)
			}
			findings := req.CheckPage(ctx, t, page, req.Fetch)
			if page.Status == 0 {
				_ = findings
				return fail(i, "goto "+step.Goto+" unreachable: "+short(page.Err))
			}

		case step.Fill != nil:
			if lastPage == nil {
				return fail(i, "fill: no page loaded yet")
			}
			doc := parseDoc(lastPage.Body)
			if doc == nil {
				return fail(i, "fill: page "+current+" has no parseable HTML")
			}
			node := QueryFirst(doc, step.Fill.Selector)
			if node == nil {
				return fail(i, fmt.Sprintf("fill: no element matches %q on %s", step.Fill.Selector, current))
			}
			name := strings.ToLower(attrOf(node, "name"))
			if name == "" {
				name = step.Fill.Selector
			}
			filled[name] = step.Fill.Value

		case step.Submit != "":
			if lastPage == nil {
				return fail(i, "submit: no page loaded yet")
			}
			doc := parseDoc(lastPage.Body)
			if doc == nil {
				return fail(i, "submit: page "+current+" has no parseable HTML")
			}
			form := formFor(doc, step.Submit)
			if form == nil {
				return fail(i, fmt.Sprintf("submit: no form matches %q on %s", step.Submit, current))
			}
			action, method := formActionMethod(form)
			actionURL := resolveURL(base, current, action)
			values := collectFormValues(form, filled)
			page, perr := req.Fetch.SubmitForm(ctx, actionURL, method, values)
			if perr != nil {
				return fail(i, "submit: "+perr.Error())
			}
			if page == nil {
				return fail(i, "submit: no response")
			}
			lastPage = page
			current = pageOrReqURL(page)
			if page.IsHTML() {
				visited = append(visited, page.FinalURL)
			}
			req.CheckPage(ctx, t, page, req.Fetch)

		case step.Click != "":
			if lastPage == nil {
				return fail(i, "click: no page loaded yet")
			}
			doc := parseDoc(lastPage.Body)
			if doc == nil {
				return fail(i, "click: page "+current+" has no parseable HTML")
			}
			node := QueryFirst(doc, step.Click)
			if node == nil {
				return fail(i, fmt.Sprintf("click: no element matches %q on %s", step.Click, current))
			}
			if node.Data == "a" {
				href := attrOf(node, "href")
				if href == "" {
					return fail(i, fmt.Sprintf("click: %q has no href", step.Click))
				}
				page, perr := req.Fetch.FetchPage(ctx, resolveURL(base, current, href))
				if perr != nil {
					return fail(i, "click: "+perr.Error())
				}
				if page == nil {
					return fail(i, "click: no response")
				}
				lastPage = page
				current = pageOrReqURL(page)
				if page.IsHTML() {
					visited = append(visited, page.FinalURL)
				}
				req.CheckPage(ctx, t, page, req.Fetch)
			} else if isSubmitControl(node) {
				form := nearestForm(node)
				if form == nil {
					return fail(i, "click: button has no form")
				}
				action, method := formActionMethod(form)
				actionURL := resolveURL(base, current, action)
				page, perr := req.Fetch.SubmitForm(ctx, actionURL, method, collectFormValues(form, filled))
				if perr != nil {
					return fail(i, "click: "+perr.Error())
				}
				if page == nil {
					return fail(i, "click: no response")
				}
				lastPage = page
				current = pageOrReqURL(page)
				if page.IsHTML() {
					visited = append(visited, page.FinalURL)
				}
				req.CheckPage(ctx, t, page, req.Fetch)
			} else {
				return fail(i, fmt.Sprintf("click: element %q is neither a link nor a submit button", step.Click))
			}

		case step.Expect != nil:
			if lastPage == nil {
				return fail(i, "expect: no page loaded yet")
			}
			if why := checkExpectation(lastPage, step.Expect); why != "" {
				return fail(i, "expectation unmet: "+why)
			}

		default: // bare think pause
			if !req.NoDelays && step.Think > 0 {
				_ = sleepCtx(ctx, step.Think.Std())
			}
		}
	}

	// All steps passed: clear the journey's issue.
	return []synthusers.Finding{{
		Type:     synthusers.TypeJourneyFail,
		Severity: synthusers.SevInfo,
		URL:      t.StartURL,
		URLKey:   scope,
		Pass:     true,
	}}, visited, nil
}

// checkExpectation validates an ExpectStep against the last response and
// returns "" when everything holds, or the reason it does not.
func checkExpectation(p *synthusers.FetchedPage, e *synthusers.ExpectStep) string {
	var unmet []string
	if e.Status != 0 && p.Status != e.Status {
		unmet = append(unmet, fmt.Sprintf("status is %d, want %d", p.Status, e.Status))
	}
	if e.Contains != "" && !strings.Contains(string(p.Body), e.Contains) {
		unmet = append(unmet, fmt.Sprintf("body does not contain %q", e.Contains))
	}
	if e.NotContains != "" && strings.Contains(string(p.Body), e.NotContains) {
		unmet = append(unmet, fmt.Sprintf("body unexpectedly contains %q", e.NotContains))
	}
	if e.Regex != "" {
		re, err := regexp.Compile(e.Regex)
		if err != nil {
			unmet = append(unmet, "invalid regex "+e.Regex)
		} else if !re.Match(p.Body) {
			unmet = append(unmet, "body does not match "+e.Regex)
		}
	}
	for name, substr := range e.Headers {
		if !strings.Contains(p.Header.Get(name), substr) {
			unmet = append(unmet, fmt.Sprintf("header %s=%q lacks %q", name, p.Header.Get(name), substr))
		}
	}
	if e.MaxMS > 0 && p.Latency.Milliseconds() > int64(e.MaxMS) {
		unmet = append(unmet, fmt.Sprintf("took %dms, max %dms", p.Latency.Milliseconds(), e.MaxMS))
	}
	return strings.Join(unmet, "; ")
}

func describeJourney(j synthusers.Journey, failedStep int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "journey %q, %d steps:", j.Name, len(j.Steps))
	for i, s := range j.Steps {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, stepLabel(s))
		if i == failedStep {
			b.WriteString("   <-- failed here")
		}
	}
	return b.String()
}

func stepLabel(s synthusers.Step) string {
	switch {
	case s.Goto != "":
		return "goto " + s.Goto
	case s.Fill != nil:
		return fmt.Sprintf("fill %s", s.Fill.Selector)
	case s.Submit != "":
		return "submit " + s.Submit
	case s.Click != "":
		return "click " + s.Click
	case s.Expect != nil:
		return "expect " + expectLabel(s.Expect)
	default:
		return "think " + s.Think.String()
	}
}

func expectLabel(e *synthusers.ExpectStep) string {
	parts := []string{}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.Status))
	}
	if e.Contains != "" {
		parts = append(parts, "contains="+e.Contains)
	}
	if e.Regex != "" {
		parts = append(parts, "regex="+e.Regex)
	}
	if e.MaxMS > 0 {
		parts = append(parts, fmt.Sprintf("max_ms=%d", e.MaxMS))
	}
	return strings.Join(parts, " ")
}

// formFor finds the form a submit selector points at: the form itself, or
// the form containing the matched element.
func formFor(doc *html.Node, sel string) *html.Node {
	if n := QueryFirst(doc, sel); n != nil {
		if n.Data == "form" {
			return n
		}
		if f := nearestForm(n); f != nil {
			return f
		}
	}
	return nil
}

func parseDoc(body []byte) *html.Node {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	return doc
}

// resolveURL turns a journey step's target into an absolute URL: absolute
// targets pass through; relative ones resolve against the current page,
// falling back to the start URL.
func resolveURL(base *url.URL, current, raw string) string {
	if u, err := url.Parse(raw); err == nil && u.IsAbs() {
		return u.String()
	}
	anchor, err := url.Parse(current)
	if err != nil || anchor.Host == "" {
		anchor = base
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return anchor.ResolveReference(ref).String()
}

func pageOrReqURL(p *synthusers.FetchedPage) string {
	if p.FinalURL != "" {
		return p.FinalURL
	}
	return p.URL
}

func short(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isSubmitControl reports whether an element acts as a form submit button.
func isSubmitControl(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.Data == "button" {
		return true
	}
	if n.Data == "input" {
		t := strings.ToLower(attrOf(n, "type"))
		return t == "" || t == "submit"
	}
	return false
}
