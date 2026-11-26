package checkers

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"
	"time"

	"synthusers"
)

// Finding aliases the engine's finding type for terser checker code.
type Finding = synthusers.Finding

// HTTPOptions tune the HTTP checker.
type HTTPOptions struct {
	// SlowThreshold flags pages slower than this. Default 3s.
	SlowThreshold time.Duration
	// SSLWarnDays warns when a certificate expires within this many days.
	// Default 14; below 3 days the finding becomes critical.
	SSLWarnDays int
}

func (o HTTPOptions) withDefaults() HTTPOptions {
	if o.SlowThreshold <= 0 {
		o.SlowThreshold = 3 * time.Second
	}
	if o.SSLWarnDays <= 0 {
		o.SSLWarnDays = 14
	}
	return o
}

// HTTP returns the checker for transport errors, HTTP statuses, page
// latency and certificate lifetime.
func HTTP(opts HTTPOptions) synthusers.Checker { return &httpChecker{opts: opts.withDefaults()} }

type httpChecker struct {
	opts HTTPOptions
}

func (c *httpChecker) Name() string { return "http" }

func (c *httpChecker) Check(ctx *synthusers.CheckContext) []Finding {
	p := ctx.Page
	pageKey := synthusers.NormalizeURLKey(p.URL)
	var out []Finding

	// Transport failure: site unreachable (DNS, refused, TLS handshake...).
	if p.Err != nil && p.Status == 0 {
		out = append(out, Finding{
			Type: synthusers.TypeHTTPStatus, Severity: synthusers.SevCritical,
			URL: p.URL, URLKey: pageKey,
			Summary: "site unreachable: " + shortErr(p.Err),
			Detail: fmt.Sprintf("error=%q latency=%s persona=%s redirects=%s",
				p.Err.Error(), p.Latency, p.Persona, strings.Join(p.Redirects, " -> ")),
		})
		return out
	}

	switch {
	case p.Status >= 500:
		out = append(out, Finding{
			Type: synthusers.TypeHTTPStatus, Severity: synthusers.SevCritical,
			URL: p.URL, URLKey: pageKey,
			Summary: fmt.Sprintf("HTTP %d on %s", p.Status, p.URL),
			Detail:  detailFor(p),
		})
	case p.Status >= 400:
		out = append(out, Finding{
			Type: synthusers.TypeHTTPStatus, Severity: synthusers.SevWarning,
			URL: p.URL, URLKey: pageKey,
			Summary: fmt.Sprintf("HTTP %d on %s", p.Status, p.URL),
			Detail:  detailFor(p),
		})
	default:
		// Healthy: clear any previously open status issue for this URL.
		out = append(out, Finding{
			Type: synthusers.TypeHTTPStatus, Severity: synthusers.SevInfo,
			URL: p.URL, URLKey: pageKey, Pass: true,
		})
	}

	// Latency.
	if p.Status != 0 && p.Latency > c.opts.SlowThreshold {
		out = append(out, Finding{
			Type: synthusers.TypeSlowResponse, Severity: synthusers.SevWarning,
			URL: p.URL, URLKey: pageKey,
			Summary: fmt.Sprintf("slow response: %s took %s (threshold %s)",
				p.URL, p.Latency.Round(time.Millisecond), c.opts.SlowThreshold),
			Detail: detailFor(p),
		})
	} else if p.Status != 0 {
		out = append(out, Finding{
			Type: synthusers.TypeSlowResponse, Severity: synthusers.SevInfo,
			URL: p.URL, URLKey: pageKey, Pass: true,
		})
	}

	out = append(out, c.sslCheck(p)...)
	return out
}

// sslCheck watches certificate lifetime; the issue scope is the site origin
// so one issue covers every page of the host.
func (c *httpChecker) sslCheck(p *synthusers.FetchedPage) []Finding {
	if p.TLS == nil || len(p.TLS.PeerCertificates) == 0 {
		return nil
	}
	cert := p.TLS.PeerCertificates[0]
	u, err := url.Parse(p.FinalURL)
	if err != nil || u.Host == "" {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	key := synthusers.NormalizeURLKey(origin)

	days := int(time.Until(cert.NotAfter).Hours() / 24)
	if days <= 0 {
		return []Finding{{
			Type: synthusers.TypeSSLExpiry, Severity: synthusers.SevCritical,
			URL: origin, URLKey: key,
			Summary: fmt.Sprintf("TLS certificate for %s expired %s ago", u.Host, time.Since(cert.NotAfter).Round(24*time.Hour)),
			Detail:  certDetail(cert, days),
		}}
	}
	if days <= 3 {
		return []Finding{{
			Type: synthusers.TypeSSLExpiry, Severity: synthusers.SevCritical,
			URL: origin, URLKey: key,
			Summary: fmt.Sprintf("TLS certificate for %s expires in %d days", u.Host, days),
			Detail:  certDetail(cert, days),
		}}
	}
	if days <= c.opts.SSLWarnDays {
		return []Finding{{
			Type: synthusers.TypeSSLExpiry, Severity: synthusers.SevWarning,
			URL: origin, URLKey: key,
			Summary: fmt.Sprintf("TLS certificate for %s expires in %d days", u.Host, days),
			Detail:  certDetail(cert, days),
		}}
	}
	return []Finding{{
		Type: synthusers.TypeSSLExpiry, Severity: synthusers.SevInfo,
		URL: origin, URLKey: key, Pass: true,
	}}
}

func certDetail(cert *x509.Certificate, days int) string {
	return fmt.Sprintf("subject=%s issuer=%s not_after=%s days_left=%d",
		cert.Subject.CommonName, cert.Issuer.CommonName,
		cert.NotAfter.UTC().Format(time.RFC3339), days)
}

func detailFor(p *synthusers.FetchedPage) string {
	parts := []string{
		"status=" + fmt.Sprint(p.Status),
		"latency=" + p.Latency.Round(time.Millisecond).String(),
		"persona=" + p.Persona,
	}
	if p.Proto != "" {
		parts = append(parts, "proto="+p.Proto)
	}
	if ct := p.ContentType(); ct != "" {
		parts = append(parts, "content_type="+ct)
	}
	if len(p.Redirects) > 0 {
		parts = append(parts, "redirects="+strings.Join(p.Redirects, " -> "))
	}
	if p.Err != nil {
		parts = append(parts, "read_error="+shortErr(p.Err))
	}
	return strings.Join(parts, " ")
}

// shortErr trims transport errors to their meaningful prefix.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 && strings.Count(s, ": ") > 2 {
		// keep the two most specific segments of deeply nested url.Errors
		seg := strings.Split(s, ": ")
		if len(seg) >= 3 {
			return strings.Join(seg[len(seg)-2:], ": ")
		}
	}
	return s
}
