// Package research is BUCKS's lightweight, READ-ONLY web researcher: it gives the
// trader plain-English market context (ticker news, "what's going on with X") by
// SEARCHING the web, FETCHING the top results read-only, and SUMMARIZING them
// through the one summary engine the chat/analyst surfaces already use — with the
// SOURCE URLS always cited. Three invariants carry in by construction:
//
//   - READ-ONLY, ENFORCED IN CODE. Every outbound request goes through a Client
//     that PHYSICALLY refuses any method that is not GET/HEAD — mirroring the
//     capability probe's roClient (probe/readonly.go). A research run can read the
//     web but can never POST/PUT/DELETE/PATCH, even on a caller bug. The same
//     ErrWriteAttempted-style closed allow-list is the safety core.
//   - SOURCES ALWAYS CITED, NEVER FABRICATED. Research returns Findings whose
//     Sources list the EXACT URLs that were actually fetched. The brief is written
//     only from text pulled off those pages (summary.Summarize over the real,
//     bounded page text). If NOTHING was fetched — no search hits, or every fetch
//     failed — Research says so plainly and returns NO brief. It never invents a
//     market take out of thin air.
//   - SINGLE PURE-GO STATIC BINARY, NO HEADLESS BROWSER. There is no chromium, no
//     cgo, no JS engine. We do a net/http read-only GET and strip the HTML to text
//     with the standard library only (no new module dependency). This means
//     JavaScript-RENDERED pages are NOT executed: this is a lightweight read-only
//     researcher over server-rendered HTML, NOT a full browser. That limit is
//     documented honestly rather than hidden.
//
// Nothing here makes a live network call in the default suite: the search provider
// is an interface (a StaticSearch / injected mock drives the tests) and fetches hit
// httptest servers. A real fetch-and-summarize path exists only behind the
// `research_live` build tag — never in the default suite.
package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrWriteAttempted is returned by the read-only Client if any caller asks it to
// issue a non-read HTTP method. Like the capability probe's identically-named
// sentinel, it exists so the read-only guarantee is enforced in CODE: the
// researcher can fetch a page but can never place, cancel, or mutate anything — the
// only methods the Client will physically send are GET and HEAD.
var ErrWriteAttempted = errors.New("research: read-only client refused a non-read HTTP method")

// readMethods is the closed allow-list of HTTP methods the researcher may issue.
// Any method not in this set is refused before a request is ever built — fail SAFE.
// Note this is TIGHTER than the probe's (no OPTIONS): research only ever GETs a page
// or HEADs it, it never needs to enumerate a server's methods.
var readMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

// Defaults for the read-only fetch client. They are sane, conservative values that
// keep a single fetch bounded in time and memory so a hostile or huge page can
// neither hang nor exhaust the process.
const (
	// DefaultMaxBytes caps how much of a page body the Client will read (2 MiB).
	// Server-rendered HTML articles are far smaller; this stops a pathological
	// response from exhausting memory.
	DefaultMaxBytes = 2 << 20 // 2 MiB
	// DefaultTimeout bounds a single fetch end-to-end.
	DefaultTimeout = 15 * time.Second
	// DefaultUserAgent is a real, honest User-Agent identifying the researcher. We
	// do not impersonate a browser; we say what we are.
	DefaultUserAgent = "bucks-research/1.0 (+read-only; net/http)"
	// defaultTextCap bounds the extracted readable text per page so a single page
	// cannot dominate the summary input. The concatenated research corpus is bounded
	// separately by Research's own cap.
	defaultTextCap = 200 * 1024 // 200 KiB of text per page
)

// Page is the readable result of a Fetch: the URL that was read, the page Title (as
// parsed from <title>, may be empty), and the Text — the visible prose with
// script/style/markup stripped and whitespace collapsed, bounded by maxText. Text is
// the ONLY thing fed to the summarizer, so the brief can never contain markup or a
// fact that was not on the page.
type Page struct {
	URL   string
	Title string
	Text  string
}

// Client is a read-only HTTP fetcher. It is PHYSICALLY incapable of issuing a write:
// every request passes through do(), which refuses any method outside the GET/HEAD
// allow-list BEFORE a request is built. It is the safety core of the researcher —
// no research code path can reach a state-changing endpoint.
type Client struct {
	http      *http.Client
	maxBytes  int64
	maxText   int
	userAgent string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient injects a custom *http.Client (e.g. a test client, or one with a
// proxy). When nil or unset, a client with DefaultTimeout is used.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.http = hc
		}
	}
}

// WithMaxBytes overrides the per-fetch body cap (bytes). Non-positive is ignored.
func WithMaxBytes(n int64) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxBytes = n
		}
	}
}

// WithMaxText overrides the per-page extracted-text cap (runes/bytes of text).
// Non-positive is ignored.
func WithMaxText(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxText = n
		}
	}
}

// WithUserAgent overrides the outbound User-Agent. Empty is ignored.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if strings.TrimSpace(ua) != "" {
			c.userAgent = ua
		}
	}
}

// NewClient builds a read-only fetch Client with sane defaults, applying any
// options. Construction makes NO network call.
func NewClient(opts ...Option) *Client {
	c := &Client{
		http:      &http.Client{Timeout: DefaultTimeout},
		maxBytes:  DefaultMaxBytes,
		maxText:   defaultTextCap,
		userAgent: DefaultUserAgent,
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: DefaultTimeout}
	}
	return c
}

// do issues a single read-only request. It REFUSES any method that is not GET/HEAD —
// returning ErrWriteAttempted WITHOUT building or sending a request — so the
// read-only guarantee holds even against a caller bug. The caller owns closing
// resp.Body.
func (c *Client) do(ctx context.Context, method, rawURL string) (*http.Response, error) {
	up := strings.ToUpper(strings.TrimSpace(method))
	if !readMethods[up] {
		return nil, fmt.Errorf("%w: %s", ErrWriteAttempted, up)
	}
	req, err := http.NewRequestWithContext(ctx, up, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("research: build %s %s: %w", up, rawURL, err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("research: %s %s: %w", up, rawURL, err)
	}
	return resp, nil
}

// Fetch GETs rawURL read-only and extracts readable TEXT from the HTML: it strips
// <script>/<style> and all tags, decodes the common HTML entities, and collapses
// whitespace, returning Page{URL, Title, Text} with Text bounded by maxText. A
// non-2xx status is a clear error (so a 404/500 is honest, never summarized as if it
// were content). The body read is bounded by maxBytes.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Page{}, errors.New("research: empty url")
	}
	resp, err := c.do(ctx, http.MethodGet, rawURL)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, fmt.Errorf("research: fetch %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := readBounded(resp.Body, c.maxBytes)
	if err != nil {
		return Page{}, fmt.Errorf("research: read %s: %w", rawURL, err)
	}
	title, text := htmlToText(string(body), c.maxText)
	return Page{URL: rawURL, Title: title, Text: text}, nil
}

// fetchRaw GETs rawURL read-only and returns the RAW, bounded body bytes WITHOUT
// stripping tags. It exists for the keyless search provider, which must parse
// <a href> result links out of the markup that Fetch's text extraction would
// destroy. It still goes through the read-only do() — the write guarantee holds —
// and is unexported because callers should normally use Fetch (stripped text).
func (c *Client) fetchRaw(ctx context.Context, rawURL string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("research: fetch %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := readBounded(resp.Body, c.maxBytes)
	if err != nil {
		return "", fmt.Errorf("research: read %s: %w", rawURL, err)
	}
	return string(body), nil
}

// Head issues a read-only HEAD and returns the status code. It exists so a caller
// can cheaply check liveness/auth without pulling a body — and it proves the HEAD
// method is part of the read-only allow-list.
func (c *Client) Head(ctx context.Context, rawURL string) (int, error) {
	resp, err := c.do(ctx, http.MethodHead, rawURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// readBounded reads up to max bytes from r (then stops), so a huge/hostile response
// cannot exhaust memory. It returns whatever was read on a mid-stream error only if
// the error is io.EOF; any other read error is surfaced.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	// LimitReader caps the bytes; +1 lets us notice (but we simply truncate at max).
	lr := io.LimitReader(r, max)
	buf, err := io.ReadAll(lr)
	if err != nil {
		return buf, err
	}
	return buf, nil
}
