# synthusers

**Synthetic-user monitoring for any website, written in Go.**

Instead of waiting for real users to hit your site's problems, synthusers
rents out a small fleet of simulated, human-like visitors that continuously
browse any website it is attached to — following links with human think
time, keeping their login sessions, replaying your critical flows — and
report every problem they bump into as a deduplicated, auto-resolving issue
on a built-in dashboard.

Think of it as Checkly / Datadog Synthetics, but self-hosted, embeddable as
a Go library, and driven by exploration instead of only hand-written checks.

```
                      ┌──────────────────────────────────────────────┐
                      │                  synthusersd                 │
   sites attach ────► │  Engine                                      │
   (API / YAML)       │   ├─ scheduler (per-target cadence, sharding)│
                      │   ├─ explorer runner  (free-roam crawler)    │
                      │   ├─ journey runner   (scripted flows)       │
                      │   ├─ checkers: http · assets · content       │
                      │   ├─ jserror runner   (headless Chrome)      │
                      │   └─ issue pipeline (fingerprint → store)    │
                      │  Dashboard: JSON API + UI   Store: pg | mem  │
                      └───────────────────┬──────────────────────────┘
                                          │ rate-limited traffic
                              ┌───────────▼───────────┐
                              │  your websites (N)    │
                              └───────────────────────┘
```

## What the users detect

| Family | Type | Severity | How |
| --- | --- | --- | --- |
| HTTP & speed | `http_status` | critical 5xx / warning 4xx | every page fetch |
| | `slow_response` | warning | latency above threshold (default 3s) |
| | `ssl_expiry` | warning <14d, critical <3d | TLS peer certificate |
| Assets | `broken_asset` | warning 4xx / critical 5xx | imgs, scripts, stylesheets, fonts, CSS `url()` refs |
| | `broken_link` | warning / critical | every `a[href]` per page |
| | `mixed_content` | critical | `http://` subresource on an `https://` page |
| Content | `content_mismatch` | warning | per-target assertions (contains / not-contains / regex / min bytes) |
| | `soft_404` | warning | HTTP 200 page that is actually a "not found" screen |
| Browser | `js_error` | critical / warning / info | uncaught exceptions, `console.error`, failed requests in a real Chrome |
| Flows | `journey_failed` | critical | a scripted journey (login, checkout…) failed a step |

Every check also emits **pass** observations, so issues resolve themselves
when the site is fixed — the dashboard shows a clean picture, not a stale
list. Findings dedupe by fingerprint (target + type + URL), so a broken logo
linked from 40 pages is **one** issue with a growing occurrence count.

## Repository layout

```
*.go (root)            core library (import "synthusers"): types, Engine,
                       scheduler, personas, rate limiting, live events,
                       Store interface, memory store
checkers/              http / assets / content detectors
explorer/              free-roaming human-like crawler
journey/               scripted flow player + small CSS selector engine
jserror/               headless-Chrome (chromedp) JS error detector
storepg/               PostgreSQL store (pgx) + embedded schema
httpui/                dashboard: JSON API + embedded single-page UI
cmd/synthusersd/       the daemon (YAML config, one binary)
cmd/brokensite/        runs the demo site
examples/brokensite/   the deliberately broken demo site (importable package)
configs/               example.yaml (every key documented), docker.yaml,
                       local.yaml (yours, git-ignored)
deploy/                Dockerfiles + docker-compose.yml (full stack)
```

## Quick start (60 seconds, no dependencies)

```bash
make build
./bin/brokensite -http :6060 -https :6161 &   # deliberately broken demo shop
./bin/synthusersd -config configs/example.yaml &  # in-memory store, port 8080
open http://localhost:8080
```

The dashboard fills with real findings within a minute: broken images and
scripts (incl. one referenced from CSS), dead links, an HTTP 500, a soft
404, mixed content on the HTTPS site, a slow endpoint, a 10-day TLS
certificate, and a failing login journey.

Run it against your own site: `cp configs/example.yaml configs/local.yaml`,
edit `targets:`, then `make run` (or POST to `/api/targets` at runtime).

## The dashboard

- **Live**: one card per synthetic user browsing right now — persona, timer,
  what it is doing ("reading /pricing… 4s", "checking links · 12 checked"),
  the trail of pages it clicked through, and every request as it completes,
  with new issues flagged the moment they are found. When idle it counts
  down to each site's next visit. Streamed over Server-Sent Events.
- **Stats cards**: open issues by severity, targets, runs and pages in 24h.
- **Issues**: filter by status / severity / type / target, expand evidence
  details, resolve manually (they also auto-resolve).
- **Targets**: attach a site at runtime, "Run now", remove.
- **Runs**: what the users did recently (pages, requests, new issues, persona).
- JSON API under `/api/*` (`/api/stats`, `/api/issues`, `/api/targets`,
  `/api/runs`, `/api/live` (SSE), `/healthz`) — everything the UI does is available to scripts.

## Configuration (`configs/example.yaml` documents every key)

```yaml
server:      { addr: :8080 }
store:       { backend: memory | postgres, dsn: postgres://... }
engine:
  interval: 5m          # default cadence per target
  workers: 8            # concurrent target runs
  rate_limit: 2         # req/s per target domain — the safety valve
  shard: 0 ; shards: 1  # fleet sharding (see below)
explorer:    { max_pages: 15, max_depth: 3 }
checks:      { slow_ms: 3000, ssl_warn_days: 14, max_assets: 25, max_links: 20 }
js:          { enabled: true, remote_url: http://chrome:9222, sample_default: 3 }
targets:
  - id: shop
    start_url: https://shop.example.com/
    mode: both            # explorer | journey | both
    interval: 10m
    visitors: 3           # a crowd: 3 personas browse at once, own cookies each
    insecure_skip_verify: false
    assertions: [ { contains: "Add to cart" } ]
    js: { sample: 3 }
    journeys:
      - name: login-and-checkout
        steps:
          - goto: /login
          - fill: { selector: "input[name=username]", value: "demo" }
          - fill: { selector: "input[name=password]", value: "hunter2" }
          - submit: "form[action=/login]"
          - expect: { contains: "Welcome back", status: 200, max_ms: 1500 }
          - click: "a[href=/logout]"
          - expect: { contains: "Signed out" }
```

Journey steps: `goto`, `fill`, `submit` (real form simulation: hidden
fields, checkboxes, selects, method/action from the markup), `click`
(links and submit buttons), `expect` (status / contains / not-contains /
regex / headers / max_ms), `think` (human pause). Selectors support
`tag`, `#id`, `.class`, `[attr]`, `[attr=v]`, `[attr*=v]` and descendant
combinators — enough for real forms without a full CSS engine.

## Using it as a Go library

```go
import (
    "synthusers"
    "synthusers/checkers"
    "synthusers/explorer"
    "synthusers/journey"
)

eng := synthusers.New(synthusers.Config{Interval: 5 * time.Minute}, synthusers.NewMemoryStore())
eng.Use(checkers.HTTP(checkers.HTTPOptions{SlowThreshold: 3 * time.Second}))
eng.Use(checkers.Assets(checkers.AssetsOptions{}))
eng.Use(checkers.Content())
eng.SetRunner("explorer", explorer.New(explorer.Options{MaxPages: 20}))
eng.SetRunner("journey", journey.New())
_ = eng.AddTarget(synthusers.Target{ID: "shop", StartURL: "https://shop.example.com/"})
eng.Start(ctx) // returns immediately; cancel ctx to stop
```

Optional pieces: `eng.UseJS(jserror.New(jserror.Options{RemoteURL: "http://chrome:9222"}))`
for browser checks, `storepg.New(ctx, dsn)` for a durable store, and
`eng.NewFetcher(target, persona)` if you write your own runner.
The core package depends only on the standard library plus
`golang.org/x/net`; every optional capability is a subpackage you wire in.

## Deployment

### Docker Compose (full stack)

```bash
make up                       # synthusersd + postgres + headless chrome + demo site
open http://localhost:8080
```

`configs/docker.yaml` wires the daemon to the Postgres sidecar and the
`chromedp/headless-shell` sidecar that provides real-browser JS checks.
Remove the `brokensite` service (`deploy/docker-compose.yml`) and the demo
target for production. `make down` stops the stack.

### Binary

```bash
make build
./bin/synthusersd -config /etc/synthusers/config.yaml
```

The binary is static (`CGO_ENABLED=0`); the distroless Docker image only
adds CA certificates.

## Scaling to 100+ sites: sharding

Run several `synthusersd` instances against the **same Postgres store**, each
with a distinct shard:

```bash
SYNTHUSERS_SHARDS=4 SYNTHUSERS_SHARD=0 ./synthusersd -config config.yaml
SYNTHUSERS_SHARDS=4 SYNTHUSERS_SHARD=1 ./synthusersd -config config.yaml
...
```

Targets are partitioned by `hash(target.id) % shards`, so every site is
visited by exactly one instance — cadence stays correct and the per-domain
rate limit (which is per instance) is never split across instances.
Restart jitter spreads targets across the interval, so a whole fleet
restarting does not thundering-herd a datacenter. The dashboard aggregates
automatically because all instances share one store.

## Being a good citizen on the sites it watches

- **Per-domain token bucket** on every outbound request (default 2 req/s,
  configurable per target), independent of think-time pacing.
- **Human think time** (2–10s per page, per persona) between page actions.
- **Budgets everywhere**: pages per run, depth, assets per page (25),
  links per page (20), one probe per URL per run (shared probe cache).
- **Crowds share the budget**: `visitors: N` runs N distinct personas at
  once, but they draw from the same per-domain token bucket, so a crowd
  never raises the request rate against the site. Only the first visitor
  runs browser (JS) checks.
- **Identity rotation**: six built-in personas (Chrome/Firefox/Safari/Edge/
  iPhone) with their own cookie jars — sessions persist like a returning
  visitor. Custom personas via config.
- Sites under test can opt out per header (`targets[].headers`), and
  `rate_limit` can be lowered per target.

## Development

```bash
make test   # unit + integration + full-stack E2E + real-Postgres store test
make vet
make run-demo   # demo site + daemon
make run        # daemon on configs/local.yaml
```

The test suite boots a real PostgreSQL 17 (embedded binaries, no Docker
needed) and runs the whole engine against the broken demo site, asserting
every issue family is detected, deduplicated, and auto-resolved after a fix.

### Notes

- The `jserror` package needs a Chrome DevTools endpoint: either a local
  Chrome/Chromium (`js.exec_path` or on `PATH`) or the headless-shell
  sidecar (`js.remote_url`). Without one, JS checks are skipped gracefully
  — everything else keeps working.
- The in-memory store resets on restart; use Postgres for history.
