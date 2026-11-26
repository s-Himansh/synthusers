package synthusers

import (
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"sync"
	"time"
)

// Persona is one simulated visitor identity: what browser it claims to be,
// which languages it accepts and how long it idles on a page between
// actions. Real sites see a small crowd of these, not one bot fingerprint.
type Persona struct {
	Name           string
	UserAgent      string
	AcceptLanguage string
	Headers        map[string]string // extra per-request headers
	ThinkMin       Duration          // idle time between page actions
	ThinkMax       Duration
}

// DefaultPersonas is the built-in crowd. Rotation happens per run: each run
// of a target is executed by one randomly chosen persona (or by one listed
// in Target.Personas).
func DefaultPersonas() []Persona {
	return []Persona{
		{
			Name:           "chrome-windows",
			UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
			AcceptLanguage: "en-US,en;q=0.9",
			ThinkMin:       Duration(2 * time.Second),
			ThinkMax:       Duration(8 * time.Second),
		},
		{
			Name:           "chrome-linux",
			UserAgent:      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
			AcceptLanguage: "en-US,en;q=0.9",
			ThinkMin:       Duration(2 * time.Second),
			ThinkMax:       Duration(7 * time.Second),
		},
		{
			Name:           "firefox-windows",
			UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:142.0) Gecko/20100101 Firefox/142.0",
			AcceptLanguage: "en-US,en;q=0.5",
			ThinkMin:       Duration(3 * time.Second),
			ThinkMax:       Duration(9 * time.Second),
		},
		{
			Name:           "safari-mac",
			UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15",
			AcceptLanguage: "en-US,en;q=0.9",
			ThinkMin:       Duration(2 * time.Second),
			ThinkMax:       Duration(8 * time.Second),
		},
		{
			Name:           "edge-windows",
			UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0",
			AcceptLanguage: "en-US,en;q=0.9",
			ThinkMin:       Duration(2 * time.Second),
			ThinkMax:       Duration(8 * time.Second),
		},
		{
			Name:           "iphone-safari",
			UserAgent:      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1",
			AcceptLanguage: "en-us",
			ThinkMin:       Duration(3 * time.Second),
			ThinkMax:       Duration(10 * time.Second),
		},
	}
}

// personaPool manages the registered personas and their per-target cookie
// jars, so a returning "visitor" keeps its session between runs (logged-in
// stays logged in), exactly like a real recurring user.
type personaPool struct {
	mu       sync.Mutex
	byName   map[string]Persona
	names    []string // sorted, for deterministic iteration
	jars     map[string]http.CookieJar
	fallback Persona
}

func newPersonaPool(custom []Persona) *personaPool {
	p := &personaPool{
		byName:   map[string]Persona{},
		jars:     map[string]http.CookieJar{},
		fallback: DefaultPersonas()[0],
	}
	for _, pr := range DefaultPersonas() {
		p.byName[pr.Name] = pr
	}
	for _, pr := range custom {
		if pr.Name == "" {
			continue
		}
		if pr.ThinkMin <= 0 {
			pr.ThinkMin = Duration(2 * time.Second)
		}
		if pr.ThinkMax < pr.ThinkMin {
			pr.ThinkMax = pr.ThinkMin + Duration(4*time.Second)
		}
		p.byName[pr.Name] = pr
	}
	for name := range p.byName {
		p.names = append(p.names, name)
	}
	sort.Strings(p.names)
	return p
}

// pick returns the persona a target's next run should use.
func (p *personaPool) pick(t *Target) Persona {
	var candidates []string
	if len(t.Personas) > 0 {
		candidates = t.Personas
	} else {
		candidates = p.names
	}
	// deterministic-ish rotation: hash target id + a time bucket
	choice := candidates[rand.Intn(len(candidates))]
	if pr, ok := p.byName[choice]; ok {
		return pr
	}
	return p.fallback
}

// jar returns (creating if needed) the cookie jar for a persona+target pair.
func (p *personaPool) jar(persona, targetID string) http.CookieJar {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := persona + "|" + targetID
	if j, ok := p.jars[key]; ok {
		return j
	}
	j, _ := cookiejar.New(nil)
	p.jars[key] = j
	return j
}

// think returns the persona's human idle time for one page.
func (p *personaPool) think(pr Persona) time.Duration {
	span := pr.ThinkMax - pr.ThinkMin
	if span <= 0 {
		return pr.ThinkMin.Std()
	}
	return pr.ThinkMin.Std() + time.Duration(rand.Int63n(int64(span.Std())))
}

// list returns all registered personas.
func (p *personaPool) list() []Persona {
	out := make([]Persona, 0, len(p.names))
	for _, n := range p.names {
		out = append(out, p.byName[n])
	}
	return out
}
