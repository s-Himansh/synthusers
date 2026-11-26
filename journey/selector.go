package journey

import (
	"strings"

	"golang.org/x/net/html"
)

// This file implements the small subset of CSS selectors journeys need:
//
//	tag            div
//	#id            #login-form
//	.class         .btn-primary
//	[attr]         [disabled]
//	[attr=value]   [name=username]
//	[attr*=value]  [action*=/login]
//	descendants    form[action*=/login] input[name=username]
//
// The last combinator is "descendant" (space). That covers realistic login,
// search and checkout forms without dragging in a full CSS engine.

// simple is one compound selector without combinators.
type simple struct {
	tag   string // "" = any
	id    string
	class []string
	attrs []attrCond
}

type attrCond struct {
	name  string
	op    string // "", "=", "*="
	value string
}

// compiled selector: sequence of compounds, joined by descendants.
type selector [][]simple

// parseSelector compiles a selector string; returns nil on syntax it
// cannot understand (callers treat that as "no match").
func parseSelector(s string) selector {
	var groups [][]simple
	for _, part := range strings.Fields(s) {
		c, ok := parseSimple(part)
		if !ok {
			return nil
		}
		groups = append(groups, []simple{c})
	}
	if len(groups) == 0 {
		return nil
	}
	return groups
}

func parseSimple(part string) (simple, bool) {
	var c simple
	i := 0
	// optional tag or *
	start := i
	for i < len(part) && (part[i] == '-' || part[i] == '_' ||
		(part[i] >= 'a' && part[i] <= 'z') || (part[i] >= 'A' && part[i] <= 'Z') ||
		(part[i] >= '0' && part[i] <= '9')) {
		i++
	}
	if i > start {
		c.tag = strings.ToLower(part[start:i])
	} else if i < len(part) && part[i] == '*' {
		i++
	}
	for i < len(part) {
		switch part[i] {
		case '#':
			i++
			start := i
			for i < len(part) && isNameChar(part[i]) {
				i++
			}
			if i == start {
				return c, false
			}
			c.id = part[start:i]
		case '.':
			i++
			start := i
			for i < len(part) && isNameChar(part[i]) {
				i++
			}
			if i == start {
				return c, false
			}
			c.class = append(c.class, part[start:i])
		case '[':
			i++
			start := i
			for i < len(part) && part[i] != ']' && part[i] != '=' && !strings.HasPrefix(part[i:], "*=") {
				i++
			}
			name := part[start:i]
			if name == "" {
				return c, false
			}
			cond := attrCond{name: strings.ToLower(name)}
			if strings.HasPrefix(part[i:], "*=") {
				cond.op = "*="
				i += 2
				vs := i
				for i < len(part) && part[i] != ']' {
					i++
				}
				cond.value = strings.Trim(part[vs:i], `"'`)
			} else if i < len(part) && part[i] == '=' {
				i++
				vs := i
				for i < len(part) && part[i] != ']' {
					i++
				}
				cond.op = "="
				cond.value = strings.Trim(part[vs:i], `"'`)
			}
			if i < len(part) && part[i] == ']' {
				i++
			} else {
				return c, false
			}
			c.attrs = append(c.attrs, cond)
		default:
			return c, false
		}
	}
	return c, true
}

func isNameChar(r byte) bool {
	return r == '-' || r == '_' || (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// matchesSimple reports whether a node satisfies one compound selector.
func matchesSimple(n *html.Node, c simple) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if c.tag != "" && c.tag != "*" && n.Data != c.tag {
		return false
	}
	if c.id != "" && attrOf(n, "id") != c.id {
		return false
	}
	if len(c.class) > 0 {
		classes := strings.Fields(attrOf(n, "class"))
		have := map[string]bool{}
		for _, cl := range classes {
			have[cl] = true
		}
		for _, want := range c.class {
			if !have[want] {
				return false
			}
		}
	}
	for _, a := range c.attrs {
		v, ok := attrLookup(n, a.name)
		if !ok {
			return false
		}
		switch a.op {
		case "=":
			if v != a.value {
				return false
			}
		case "*=":
			if !strings.Contains(v, a.value) {
				return false
			}
		}
	}
	return true
}

// matches reports whether a node satisfies the whole selector (including
// the descendant requirements).
func matches(n *html.Node, sel selector) bool {
	if len(sel) == 0 {
		return false
	}
	last := sel[len(sel)-1]
	if !matchAllSimple(n, last[0]) {
		return false
	}
	// walk up for the remaining compounds
	node := n.Parent
	group := len(sel) - 2
	for group >= 0 && node != nil {
		if matchAllSimple(node, sel[group][0]) {
			group--
		}
		node = node.Parent
	}
	return group < 0
}

func matchAllSimple(n *html.Node, c simple) bool { return matchesSimple(n, c) }

// QueryAll returns all nodes matching sel, in document order.
func QueryAll(root *html.Node, sel string) []*html.Node {
	parsed := parseSelector(sel)
	if parsed == nil {
		return nil
	}
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if matches(n, parsed) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// QueryFirst returns the first matching node or nil.
func QueryFirst(root *html.Node, sel string) *html.Node {
	nodes := QueryAll(root, sel)
	if len(nodes) == 0 {
		return nil
	}
	return nodes[0]
}

func attrOf(n *html.Node, key string) string {
	v, _ := attrLookup(n, key)
	return v
}

func attrLookup(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}
