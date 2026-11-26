package jserror

import (
	"context"
	"fmt"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"synthusers"
)

// findingFor turns one console message into a finding; ok=false skips it.
func findingFor(m chromedp.ConsoleMessage, pageURL string) (synthusers.Finding, bool) {
	key := synthusers.NormalizeURLKey(pageURL)
	loc := ""
	if m.URL != "" {
		loc = fmt.Sprintf(" (%s:%d:%d)", m.URL, m.Line+1, m.Column+1)
	}
	switch {
	case m.Type == chromedp.ConsoleException:
		return synthusers.Finding{
			Type: synthusers.TypeJSError, Severity: synthusers.SevCritical,
			URL: pageURL, URLKey: key,
			Summary: "uncaught JS exception on " + pageURL + ": " + truncate(m.Text, 300) + loc,
		}, true
	case m.Type == chromedp.ConsoleError:
		return synthusers.Finding{
			Type: synthusers.TypeJSError, Severity: synthusers.SevWarning,
			URL: pageURL, URLKey: key,
			Summary: "console error on " + pageURL + ": " + truncate(m.Text, 300) + loc,
		}, true
	case m.Source != "" && m.Type == "error":
		// Browser log errors (e.g. source "network"): failed requests the
		// page's own JS triggered.
		return synthusers.Finding{
			Type: synthusers.TypeJSError, Severity: synthusers.SevInfo,
			URL: pageURL, URLKey: key,
			Summary: fmt.Sprintf("browser error (%s) on %s: %s", m.Source, pageURL, truncate(m.Text, 300)),
		}, true
	}
	return synthusers.Finding{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// enableDomain wraps a CDP command into a chromedp action.
func enableDomain[P, R any](cmd cdp.Command[P, R], params P) chromedp.Action[chromedp.Void] {
	return chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		_, err := cdp.Call(ctx, t, cmd, params)
		return err
	})
}

// setUserAgent applies a persona's User-Agent to the tab.
func setUserAgent(ua string) chromedp.Action[chromedp.Void] {
	return chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		if ua == "" {
			return nil
		}
		_, err := cdp.Call(ctx, t, emulation.SetUserAgentOverride,
			emulation.SetUserAgentOverrideParams{UserAgent: ua})
		return err
	})
}

var (
	logEnable     = log.Enable
	networkEnable = network.Enable
)
