package synthusers

import (
	"context"
	"net/http"
	"sync"
	"time"
) // rateLimiter enforces a per-domain requests/second budget on every outbound
// request the engine makes (pages, link probes, asset probes, journeys). It
// is the guardrail that makes "a crowd of simulated users" safe to point at
// a production site: each distinct host gets its own token bucket, so
// monitoring site A never slows down probes of site B.
type rateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*domainBucket
	defaultRPS float64 // requests per second per domain
}

type domainBucket struct {
	interval time.Duration // min time between requests
	last     time.Time
}

func newRateLimiter(defaultRPS float64) *rateLimiter {
	if defaultRPS <= 0 {
		defaultRPS = 2
	}
	return &rateLimiter{buckets: map[string]*domainBucket{}, defaultRPS: defaultRPS}
}

// acquire blocks until the domain's next slot is available or ctx is done.
func (rl *rateLimiter) acquire(ctx context.Context, host string, rps float64) error {
	if rps <= 0 {
		rps = rl.defaultRPS
	}
	rl.mu.Lock()
	b, ok := rl.buckets[host]
	if !ok {
		b = &domainBucket{interval: time.Duration(float64(time.Second) / rps)}
		rl.buckets[host] = b
	}
	wait := time.Duration(0)
	if !b.last.IsZero() {
		next := b.last.Add(b.interval)
		if next.After(time.Now()) {
			wait = time.Until(next)
		}
	}
	b.last = time.Now().Add(wait) // reserve the slot
	rl.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// limitingTransport is an http.RoundTripper that rate-limits by request
// host. Per-request overrides arrive via the rateLimitKey context value.
type limitingTransport struct {
	base    http.RoundTripper
	limiter *rateLimiter
	// per-request rate limit in req/s, or 0 to use the default
}

type ctxKeyRPS struct{}

// WithRPS returns a context carrying a per-request rate limit override.
func WithRPS(ctx context.Context, rps float64) context.Context {
	return context.WithValue(ctx, ctxKeyRPS{}, rps)
}

func (lt *limitingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rps := 0.0
	if v, ok := req.Context().Value(ctxKeyRPS{}).(float64); ok {
		rps = v
	} else if v, ok := req.Context().Value(ctxKeyRPS{}).(float32); ok {
		rps = float64(v)
	}
	if err := lt.limiter.acquire(req.Context(), req.URL.Host, rps); err != nil {
		return nil, err
	}
	return lt.base.RoundTrip(req)
}
