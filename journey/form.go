package journey

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// collectFormValues assembles the request payload of a <form> the way a
// browser would: every named field contributes its current value, with
// values the journey filled via "fill" steps taking precedence.
func collectFormValues(form *html.Node, filled map[string]string) url.Values {
	values := url.Values{}
	if form == nil {
		return values
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "input":
				name := strings.ToLower(attrOf(n, "name"))
				typ := strings.ToLower(attrOf(n, "type"))
				if name == "" || typ == "submit" || typ == "button" || typ == "image" || typ == "file" || typ == "reset" {
					break
				}
				switch typ {
				case "checkbox", "radio":
					if hasAttr(n, "checked") {
						v := attrOf(n, "value")
						if v == "" {
							v = "on"
						}
						values.Set(name, v)
					}
				default:
					values.Set(name, fieldValue(n, filled))
				}
			case "select":
				name := strings.ToLower(attrOf(n, "name"))
				if name == "" {
					break
				}
				values.Set(name, selectValue(n, filled))
			case "textarea":
				name := strings.ToLower(attrOf(n, "name"))
				if name == "" {
					break
				}
				if v, ok := filled[name]; ok {
					values.Set(name, v)
				} else {
					values.Set(name, textContent(n))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return values
}

// fieldValue resolves an input's value: journey-filled first, then the
// value attribute.
func fieldValue(n *html.Node, filled map[string]string) string {
	name := strings.ToLower(attrOf(n, "name"))
	if v, ok := filled[name]; ok {
		return v
	}
	return attrOf(n, "value")
}

// selectValue picks the option a browser would send: the journey-filled
// value if it matches an option, else the selected option, else the first.
func selectValue(sel *html.Node, filled map[string]string) string {
	name := strings.ToLower(attrOf(sel, "name"))
	fill, hasFill := filled[name]

	var first, selected string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "option" {
			v := attrOf(n, "value")
			if v == "" {
				v = textContent(n)
			}
			if first == "" {
				first = v
			}
			if hasFill && (v == fill || textContent(n) == fill) {
				selected = v
			}
			if selected == "" && hasAttr(n, "selected") {
				selected = v
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(sel)
	if selected != "" {
		return selected
	}
	return first
}

func textContent(n *html.Node) string {
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

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

// nearestForm climbs to the form owning a field; nil when none.
func nearestForm(n *html.Node) *html.Node {
	for p := n; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "form" {
			return p
		}
	}
	return nil
}

// formActionMethod extracts the form's action URL (raw) and method.
func formActionMethod(form *html.Node) (action, method string) {
	action = attrOf(form, "action")
	method = strings.ToUpper(attrOf(form, "method"))
	if method == "" {
		method = "GET"
	}
	return action, method
}
