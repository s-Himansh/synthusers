// Package checkers implements the page-level problem detectors of the
// synthetic users: HTTP status/latency/TLS, broken assets and links, mixed
// content and content assertions/soft-404s. Each checker implements the
// synthusers.Checker interface and is wired with Engine.Use.
package checkers

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// parseHTML parses a response body into an HTML document, tolerating
// malformed markup the way browsers do.
func parseHTML(body []byte) (*html.Node, error) {
	r := bytes.NewReader(body)
	return html.Parse(r)
}

// docText concatenates the text content of a subtree.
func docText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// titleOf returns the <title> text, "" if absent.
func titleOf(doc *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "title" {
			found = docText(n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// firstH1 returns the text of the first <h1> heading, "" if absent.
func firstH1(doc *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "h1" {
			found = docText(n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// Ref is one subresource reference extracted from an HTML document.
type Ref struct {
	URL  string // absolute URL after resolution
	Kind string // "asset" or "link"
	Tag  string // img, script, link, iframe, a, ...
	Attr string // src, href, srcset, poster, ...
}

// assetTags maps HTML tags to the attributes that carry subresource URLs.
var assetTags = map[string][]string{
	"img":    {"src", "srcset"},
	"script": {"src"},
	"link":   {"href"},
	"iframe": {"src"},
	"source": {"src", "srcset"},
	"video":  {"src", "poster"},
	"audio":  {"src"},
	"embed":  {"src"},
	"object": {"data"},
}

// skipSchemes are non-fetchable URL schemes; references using them are
// never probed.
var skipSchemes = map[string]bool{
	"data": true, "javascript": true, "mailto": true, "tel": true,
	"about": true, "blob": true, "intent": true, "sms": true,
}

// extractRefs walks a document and returns every probed-able reference,
// resolved against baseURL. Anchors-only URLs (e.g. "#top") are dropped.
func extractRefs(doc *html.Node, baseURL *url.URL) []Ref {
	var out []Ref
	seen := map[string]bool{}
	add := func(raw, kind, tag, attr string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" && u.Host == "" && u.Path == "" {
			return
		}
		if skipSchemes[strings.ToLower(u.Scheme)] {
			return
		}
		abs := baseURL.ResolveReference(u)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			return
		}
		abs.Fragment = ""
		key := abs.String() + "|" + kind
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Ref{URL: abs.String(), Kind: kind, Tag: tag, Attr: attr})
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if attrs, ok := assetTags[n.Data]; ok {
				for _, a := range attrs {
					if v := attrValue(n, a); v != "" {
						if a == "srcset" {
							v = srcsetFirst(v)
						}
						add(v, "asset", n.Data, a)
					}
				}
			}
			if n.Data == "a" {
				if v := attrValue(n, "href"); v != "" {
					add(v, "link", "a", "href")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// ExtractLinks returns the absolute URLs of all a[href] links in an HTML
// body (deduplicated, fragments stripped). Exported for the explorer
// crawler.
func ExtractLinks(body []byte, baseURL *url.URL) []string {
	doc, err := parseHTML(body)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			if v := attrValue(n, "href"); v != "" && !strings.HasPrefix(v, "#") {
				u, err := url.Parse(strings.TrimSpace(v))
				if err == nil && !skipSchemes[strings.ToLower(u.Scheme)] {
					abs := baseURL.ResolveReference(u)
					abs.Fragment = ""
					if (abs.Scheme == "http" || abs.Scheme == "https") && !seen[abs.String()] {
						seen[abs.String()] = true
						out = append(out, abs.String())
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// srcsetFirst returns the first candidate URL of a srcset attribute.
func srcsetFirst(v string) string {
	if i := strings.IndexAny(v, " \t,"); i >= 0 {
		return v[:i]
	}
	return v
}

// cssURLs extracts every url(...) and @import reference from a stylesheet.
func cssURLs(css []byte) []string {
	var out []string
	seen := map[string]bool{}
	push := func(s string) {
		s = strings.Trim(s, `"'`)
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	s := string(css)
	for i := 0; i+4 <= len(s); {
		switch {
		case strings.HasPrefix(s[i:], "url("):
			j := strings.IndexByte(s[i+4:], ')')
			if j < 0 {
				i += 4
				continue
			}
			push(s[i+4 : i+4+j])
			i += 4 + j + 1
		case strings.HasPrefix(s[i:], "@import"):
			rest := s[i+7:]
			for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
				rest = rest[1:]
			}
			if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
				q := rest[0]
				if j := strings.IndexByte(rest[1:], q); j >= 0 {
					push(rest[1 : 1+j])
					i = len(s) - len(rest[1+j+1:])
					continue
				}
			}
			i += 7
		default:
			i++
		}
	}
	return out
}
