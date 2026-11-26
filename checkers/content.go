package checkers

import (
	"fmt"
	"regexp"
	"strings"

	"synthusers"
)

// Content returns the checker that validates page content: the target's
// assertions (must-contain / must-not-contain / regex / minimum size) and a
// soft-404 detector for pages that answer 200 OK with a "not found" screen.
func Content() synthusers.Checker { return &contentChecker{} }

type contentChecker struct {
	soft404Title *regexp.Regexp
	soft404H1    *regexp.Regexp
}

func (c *contentChecker) ensurePatterns() {
	if c.soft404Title == nil {
		c.soft404Title = regexp.MustCompile(`(?i)\b(404|403|500|server error|page not found|not found|no longer available|oops|something went wrong)\b`)
		c.soft404H1 = regexp.MustCompile(`(?i)^(404|not found|page not found|error( \d+)?|oops|empty|no results found)$`)
	}
}

func (c *contentChecker) Name() string { return "content" }

func (c *contentChecker) Check(ctx *synthusers.CheckContext) []Finding {
	p := ctx.Page
	if p.Status == 0 || p.Status >= 400 {
		return nil // broken transport/status is the http checker's job
	}
	if !p.IsHTML() {
		return nil
	}
	doc, err := parseHTML(p.Body)
	if err != nil {
		return nil
	}
	pageKey := synthusers.NormalizeURLKey(p.URL)
	var out []Finding

	// Assertions from the target.
	var failures []string
	for _, a := range ctx.Target.Assertions {
		switch {
		case a.Contains != "" && !strings.Contains(string(p.Body), a.Contains):
			failures = append(failures, "body does not contain "+quote(a.Contains))
		case a.NotContains != "" && strings.Contains(string(p.Body), a.NotContains):
			failures = append(failures, "body unexpectedly contains "+quote(a.NotContains))
		case a.Regex != "":
			re, err := regexp.Compile(a.Regex)
			if err != nil {
				failures = append(failures, "invalid assertion regex "+quote(a.Regex))
				continue
			}
			if !re.Match(p.Body) {
				failures = append(failures, "body does not match "+quote(a.Regex))
			}
		case a.MinBytes > 0 && len(p.Body) < a.MinBytes:
			failures = append(failures, fmt.Sprintf("body is %d bytes, assertion requires >= %d", len(p.Body), a.MinBytes))
		}
	}
	if len(failures) > 0 {
		out = append(out, Finding{
			Type: synthusers.TypeContentFail, Severity: synthusers.SevWarning,
			URL: p.URL, URLKey: pageKey,
			Summary: "content assertion failed on " + p.URL + ": " + strings.Join(failures, "; "),
			Detail:  joinDetails(failures, len(p.Body), titleOf(doc)),
		})
	} else if len(ctx.Target.Assertions) > 0 {
		out = append(out, Finding{
			Type: synthusers.TypeContentFail, Severity: synthusers.SevInfo,
			URL: p.URL, URLKey: pageKey, Pass: true,
		})
	}

	// Soft 404: HTTP 200 with a "not found" face.
	c.ensurePatterns()
	title := titleOf(doc)
	h1 := firstH1(doc)
	if c.soft404Title.MatchString(title) || c.soft404H1.MatchString(strings.TrimSpace(h1)) {
		out = append(out, Finding{
			Type: synthusers.TypeSoft404, Severity: synthusers.SevWarning,
			URL: p.URL, URLKey: pageKey,
			Summary: "soft 404 on " + p.URL + " (HTTP 200, page says not found)",
			Detail:  fmt.Sprintf("title=%q h1=%q bytes=%d", title, h1, len(p.Body)),
		})
	} else {
		out = append(out, Finding{
			Type: synthusers.TypeSoft404, Severity: synthusers.SevInfo,
			URL: p.URL, URLKey: pageKey, Pass: true,
		})
	}
	return out
}

func quote(s string) string { return "\"" + s + "\"" }

func joinDetails(failures []string, size int, title string) string {
	return fmt.Sprintf("failures=%d size=%d bytes title=%q details: %s",
		len(failures), size, title, strings.Join(failures, "; "))
}
