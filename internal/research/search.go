package research

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Result is one search hit: a human Title, the URL to fetch, and a short Snippet of
// context. Only URL is load-bearing for the researcher (it is what gets fetched);
// Title/Snippet improve the cited-source display.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// SearchProvider is the pluggable search seam. A user can wire ANY provider — the
// keyless best-effort default below, or a keyed Brave/Serper/Tavily client they
// supply later — as long as it satisfies this one method. No provider key is
// embedded anywhere in BUCKS.
type SearchProvider interface {
	// Search returns ranked results for query, or an error. It must not panic on a
	// transport error — return it, so Research can report "no sources" honestly.
	Search(ctx context.Context, query string) ([]Result, error)
}

// StaticSearch is a fixed, injected provider: it returns the same Results for any
// query. It is the deterministic provider the default test suite drives (no network)
// and is also handy as a "here are the pages I already know" provider.
type StaticSearch struct {
	Results []Result
	// Err, when set, is returned instead of Results — lets a test exercise the
	// search-failed honesty path.
	Err error
}

// Search returns the static results (or the configured error).
func (s StaticSearch) Search(_ context.Context, _ string) ([]Result, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	return s.Results, nil
}

// SearchFunc adapts a plain function to a SearchProvider — convenient for tests and
// for wrapping a user's own keyed client without declaring a type.
type SearchFunc func(ctx context.Context, query string) ([]Result, error)

// Search calls the wrapped function.
func (f SearchFunc) Search(ctx context.Context, query string) ([]Result, error) {
	if f == nil {
		return nil, errors.New("research: nil SearchFunc")
	}
	return f(ctx, query)
}

// DuckDuckGoSearch is the KEYLESS, BEST-EFFORT default search provider. It scrapes
// the DuckDuckGo HTML-lite endpoint (https://html.duckduckgo.com/html/) read-only
// through the same read-only Client, and parses result links/titles from the
// returned HTML.
//
// HONESTY / LIMITS: this is explicitly best-effort and FLAKY. The endpoint is not a
// stable API — its markup can change, it may rate-limit or block automated access,
// and it returns no machine-readable contract. When it changes or blocks us, Search
// returns an error (or zero results) and the researcher reports "no sources" rather
// than inventing any. For a reliable, supported search, a user should plug a keyed
// provider (Brave/Serper/Tavily) via the SearchProvider interface — no key is
// embedded here. This default exists so `bucks research` works with zero setup, not
// because it is robust.
type DuckDuckGoSearch struct {
	// Client is the read-only fetcher used to GET the HTML endpoint. Required.
	Client *Client
	// MaxResults caps how many parsed results to return (0 => a sane default).
	MaxResults int
}

// ddgEndpoint is the HTML-lite endpoint. Documented best-effort; no key, read-only.
const ddgEndpoint = "https://html.duckduckgo.com/html/"

// Search runs the best-effort keyless query. It GETs the HTML-lite endpoint
// read-only and extracts result links. Any transport failure or empty parse is
// surfaced (no fabrication).
func (d DuckDuckGoSearch) Search(ctx context.Context, query string) ([]Result, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("research: empty search query")
	}
	if d.Client == nil {
		return nil, errors.New("research: DuckDuckGoSearch needs a read-only Client")
	}
	endpoint := ddgEndpoint + "?q=" + url.QueryEscape(q)
	// Reuse the read-only Client.Fetch but we want the RAW html, not the stripped
	// text — so fetch then parse links from the raw body. Fetch already strips tags,
	// which would destroy the <a href> links we need; use a dedicated raw read.
	raw, err := d.Client.fetchRaw(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("research: ddg best-effort search failed: %w", err)
	}
	results := parseDDGResults(raw)
	max := d.MaxResults
	if max <= 0 {
		max = 5
	}
	if len(results) > max {
		results = results[:max]
	}
	if len(results) == 0 {
		// Honest: the endpoint answered but we parsed nothing (markup changed or it
		// blocked us). Report it, don't invent.
		return nil, errors.New("research: ddg best-effort search returned no parseable results (endpoint may have changed or blocked automated access — plug a keyed provider for reliable search)")
	}
	return results, nil
}

// ddgResultRe captures result anchors from the DDG HTML-lite page. The lite page
// uses <a class="result__a" href="...">Title</a>. This is best-effort: if the markup
// changes, this matches nothing and Search reports zero results honestly.
var ddgResultRe = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)

// parseDDGResults pulls result links + titles from the raw DDG HTML-lite page.
// DDG often wraps the real target in a redirect (uddg=<encoded-url>); we unwrap that
// to the real URL so the fetcher reads the actual page, not a redirector.
func parseDDGResults(rawHTML string) []Result {
	matches := ddgResultRe.FindAllStringSubmatch(rawHTML, -1)
	var out []Result
	seen := map[string]bool{}
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		href := unwrapDDGURL(strings.TrimSpace(m[1]))
		if href == "" || seen[href] {
			continue
		}
		_, title := htmlToText(m[2], 256)
		seen[href] = true
		out = append(out, Result{Title: strings.TrimSpace(title), URL: href})
	}
	return out
}

// unwrapDDGURL turns DDG's redirect wrapper into the real destination. DDG lite
// links look like "//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2F..." — we
// decode the uddg param. A plain absolute http(s) URL is returned as-is. Anything we
// cannot resolve to an http(s) URL is dropped (returns "").
func unwrapDDGURL(href string) string {
	if href == "" {
		return ""
	}
	// Add a scheme for protocol-relative links so url.Parse can read the query.
	parseTarget := href
	if strings.HasPrefix(href, "//") {
		parseTarget = "https:" + href
	}
	u, err := url.Parse(parseTarget)
	if err == nil {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			if real, derr := url.QueryUnescape(uddg); derr == nil && isHTTPURL(real) {
				return real
			}
		}
	}
	if isHTTPURL(href) {
		return href
	}
	return ""
}

// isHTTPURL reports whether s is an absolute http or https URL.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
