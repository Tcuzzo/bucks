package research

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bucks/internal/analyst"
)

// --- mock backend (no network) -------------------------------------------------

// scriptBackend is a deterministic analyst.Backend: it records the LAST prompt it
// saw (so a test can assert the brief was built from the fetched corpus) and returns
// a fixed reply, or a fixed error.
type scriptBackend struct {
	name       string
	reply      string
	err        error
	lastPrompt string
}

func (b *scriptBackend) Name() string { return b.name }

func (b *scriptBackend) Complete(_ context.Context, prompt string) (string, error) {
	b.lastPrompt = prompt
	if b.err != nil {
		return "", b.err
	}
	return b.reply, nil
}

// --- read-only enforcement -----------------------------------------------------

// TestClient_RefusesNonReadMethods proves the read-only guarantee is enforced IN
// CODE: a POST/PUT/DELETE/PATCH is refused with ErrWriteAttempted BEFORE any request
// is sent. A test server is wired but must receive ZERO of these methods.
func TestClient_RefusesNonReadMethods(t *testing.T) {
	var mu sync.Mutex
	var sawMethods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawMethods = append(sawMethods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(WithHTTPClient(srv.Client()))
	ctx := context.Background()

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, "CONNECT", "TRACE"} {
		_, err := c.do(ctx, m, srv.URL)
		if !errors.Is(err, ErrWriteAttempted) {
			t.Errorf("method %s: expected ErrWriteAttempted, got %v", m, err)
		}
	}

	// The allowed reads must actually go through.
	if _, err := c.Fetch(ctx, srv.URL); err != nil {
		t.Fatalf("GET should be allowed: %v", err)
	}
	if _, err := c.Head(ctx, srv.URL); err != nil {
		t.Fatalf("HEAD should be allowed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range sawMethods {
		if m != http.MethodGet && m != http.MethodHead {
			t.Fatalf("server received a non-read method %q — read-only guarantee broken", m)
		}
	}
}

// TestResearch_NeverSendsWriteMethods proves an END-TO-END research run touches the
// test servers with ONLY GET/HEAD: the server asserts it never sees a write.
func TestResearch_NeverSendsWriteMethods(t *testing.T) {
	var mu sync.Mutex
	var nonRead int
	page := htmlServer(t, "<title>Acme</title><p>Acme stock rose on strong earnings.</p>", func(method string) {
		mu.Lock()
		defer mu.Unlock()
		if method != http.MethodGet && method != http.MethodHead {
			nonRead++
		}
	})
	defer page.Close()

	c := NewClient(WithHTTPClient(page.Client()))
	sp := StaticSearch{Results: []Result{{Title: "Acme", URL: page.URL}}}
	be := &scriptBackend{name: "mock", reply: "Acme rose on earnings."}

	if _, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "acme stock"); err != nil {
		t.Fatalf("Research: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if nonRead != 0 {
		t.Fatalf("research run issued %d non-read method(s) — read-only broken", nonRead)
	}
}

// --- fetch / html extraction ---------------------------------------------------

// TestFetch_ExtractsTextStripsScriptStyleAndTags proves Fetch pulls the Title and
// the readable text, with <script>/<style> bodies and all tags removed and
// whitespace collapsed. The bound is exercised too.
func TestFetch_ExtractsTextStripsScriptStyleAndTags(t *testing.T) {
	html := `<!DOCTYPE html><html><head><title>  Earnings  Beat  </title>
		<style>.x{color:red} body{font:12px}</style></head>
		<body>
		<script>var secret = "do-not-leak"; alert(1)</script>
		<h1>Acme&amp;Co</h1>
		<p>Revenue   grew   20%.</p>
		<!-- hidden comment should vanish -->
		<div>Guidance raised for next quarter.</div>
		</body></html>`
	srv := htmlServer(t, html, nil)
	defer srv.Close()

	c := NewClient(WithHTTPClient(srv.Client()))
	page, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.Title != "Earnings Beat" {
		t.Errorf("Title = %q, want %q", page.Title, "Earnings Beat")
	}
	// Script/style content must NOT appear.
	for _, leak := range []string{"do-not-leak", "alert(1)", "color:red", "font:12px", "hidden comment"} {
		if strings.Contains(page.Text, leak) {
			t.Errorf("Text leaked non-prose %q:\n%s", leak, page.Text)
		}
	}
	// Prose (entity-decoded) must appear.
	for _, want := range []string{"Acme&Co", "Revenue grew 20%.", "Guidance raised for next quarter."} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("Text missing %q:\n%s", want, page.Text)
		}
	}
	// No raw tags survive.
	if strings.Contains(page.Text, "<") || strings.Contains(page.Text, ">") {
		t.Errorf("Text still contains markup:\n%s", page.Text)
	}
}

// TestFetch_BoundsText proves the per-page text cap truncates a huge page.
func TestFetch_BoundsText(t *testing.T) {
	big := "<p>" + strings.Repeat("word ", 5000) + "</p>"
	srv := htmlServer(t, big, nil)
	defer srv.Close()

	c := NewClient(WithHTTPClient(srv.Client()), WithMaxText(100))
	page, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(page.Text) > 100 {
		t.Errorf("Text not bounded: len=%d > 100", len(page.Text))
	}
	if page.Text == "" {
		t.Error("bounded text should be non-empty")
	}
}

// TestFetch_Non2xxIsError proves a 404/500 is an honest error, never summarized as
// content.
func TestFetch_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(WithHTTPClient(srv.Client()))
	if _, err := c.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error on 404")
	}
}

// --- research: sources always cited -------------------------------------------

// TestResearch_FetchesTopNAndCitesAllSources proves the happy path: a mock provider
// returns TWO httptest URLs, BOTH are fetched, the text is summarized via a mock
// backend, and Findings.Brief is non-empty AND Findings.Sources lists BOTH URLs.
func TestResearch_FetchesTopNAndCitesAllSources(t *testing.T) {
	a := htmlServer(t, "<title>News A</title><p>Acme beat earnings by a wide margin.</p>", nil)
	defer a.Close()
	b := htmlServer(t, "<title>News B</title><p>Analysts raised Acme price targets.</p>", nil)
	defer b.Close()

	c := NewClient(WithHTTPClient(sharedClient(a, b)))
	sp := StaticSearch{Results: []Result{
		{Title: "A", URL: a.URL},
		{Title: "B", URL: b.URL},
	}}
	be := &scriptBackend{name: "mock", reply: "Acme beat earnings and analysts raised targets."}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "acme earnings", WithTopN(2))
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if strings.TrimSpace(f.Brief) == "" {
		t.Error("Brief should be non-empty when pages were fetched")
	}
	if !f.HasSources() || len(f.Sources) != 2 {
		t.Fatalf("expected 2 cited sources, got %v", f.Sources)
	}
	// BOTH source URLs must be cited.
	for _, want := range []string{a.URL, b.URL} {
		found := false
		for _, s := range f.Sources {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("source %q not cited; sources=%v", want, f.Sources)
		}
	}
	// The brief must be built from the FETCHED corpus, not the query alone: the mock
	// backend saw the page text in its prompt.
	if !strings.Contains(be.lastPrompt, "beat earnings") || !strings.Contains(be.lastPrompt, "price targets") {
		t.Errorf("summarizer prompt did not contain the fetched page text:\n%s", be.lastPrompt)
	}
	if f.Backend != "mock" {
		t.Errorf("Backend = %q, want mock", f.Backend)
	}
}

// TestResearch_RespectsTopN proves only the top-N results are fetched even when the
// provider returns more.
func TestResearch_RespectsTopN(t *testing.T) {
	a := htmlServer(t, "<p>page one content here</p>", nil)
	defer a.Close()
	b := htmlServer(t, "<p>page two content here</p>", nil)
	defer b.Close()

	c := NewClient(WithHTTPClient(sharedClient(a, b)))
	sp := StaticSearch{Results: []Result{{URL: a.URL}, {URL: b.URL}}}
	be := &scriptBackend{name: "mock", reply: "brief"}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "q", WithTopN(1))
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if len(f.Sources) != 1 || f.Sources[0] != a.URL {
		t.Fatalf("WithTopN(1) should cite only the first source, got %v", f.Sources)
	}
}

// --- honesty: no sources => no fabricated brief --------------------------------

// TestResearch_NoSearchResults_NoFabrication proves that with zero search results,
// Research returns a clear "no sources" outcome and NO fabricated brief.
func TestResearch_NoSearchResults_NoFabrication(t *testing.T) {
	c := NewClient()
	sp := StaticSearch{Results: nil}
	be := &scriptBackend{name: "mock", reply: "FABRICATED MARKET TAKE"}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "no such ticker")
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if f.HasSources() {
		t.Fatalf("expected no sources, got %v", f.Sources)
	}
	if strings.Contains(f.Brief, "FABRICATED MARKET TAKE") {
		t.Fatal("Research fabricated a brief from the model despite zero sources")
	}
	if !strings.Contains(strings.ToLower(f.Brief), "couldn't find") {
		t.Errorf("expected an honest no-sources brief, got: %q", f.Brief)
	}
	if be.lastPrompt != "" {
		t.Error("the model must NOT be called when there are no sources")
	}
}

// TestResearch_AllFetchesFail_NoFabrication proves that when search returns URLs but
// every fetch fails (all 500s), Research returns "no sources" and no brief from the
// model — never invented content.
func TestResearch_AllFetchesFail_NoFabrication(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	c := NewClient(WithHTTPClient(dead.Client()))
	sp := StaticSearch{Results: []Result{{URL: dead.URL}, {URL: dead.URL + "/2"}}}
	be := &scriptBackend{name: "mock", reply: "FABRICATED"}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "q", WithTopN(2))
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if f.HasSources() {
		t.Fatalf("no fetch succeeded; expected no sources, got %v", f.Sources)
	}
	if strings.Contains(f.Brief, "FABRICATED") {
		t.Fatal("Research fabricated a brief despite all fetches failing")
	}
	if len(f.Errors) == 0 {
		t.Error("expected per-source fetch errors to be recorded")
	}
	if be.lastPrompt != "" {
		t.Error("the model must NOT be called when no page was readable")
	}
}

// TestResearch_SearchProviderError_NoFabrication proves a provider that ERRORS yields
// an honest no-sources outcome (not a crash, not a fabricated brief).
func TestResearch_SearchProviderError_NoFabrication(t *testing.T) {
	c := NewClient()
	sp := StaticSearch{Err: errors.New("provider down")}
	be := &scriptBackend{name: "mock", reply: "FABRICATED"}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "q")
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if f.HasSources() || strings.Contains(f.Brief, "FABRICATED") {
		t.Fatalf("provider error must yield no sources and no fabricated brief; got brief=%q sources=%v", f.Brief, f.Sources)
	}
	if len(f.Errors) == 0 || !strings.Contains(f.Errors[0], "search failed") {
		t.Errorf("expected a recorded search failure, got %v", f.Errors)
	}
}

// TestResearch_SummarizerFailsAfterFetch_SourcesStillCited proves that if pages WERE
// fetched but the model fails, Research returns an error AND still cites the sources
// it read (nothing hidden, nothing invented).
func TestResearch_SummarizerFailsAfterFetch_SourcesStillCited(t *testing.T) {
	a := htmlServer(t, "<p>real content was read here</p>", nil)
	defer a.Close()

	c := NewClient(WithHTTPClient(a.Client()))
	sp := StaticSearch{Results: []Result{{URL: a.URL}}}
	be := &scriptBackend{name: "mock", err: errors.New("model 503")}

	f, err := Research(context.Background(), sp, c, []analyst.Backend{be}, "q")
	if err == nil {
		t.Fatal("expected an error when the only backend fails after fetch")
	}
	if !f.HasSources() || f.Sources[0] != a.URL {
		t.Fatalf("sources read must still be cited on summarizer failure, got %v", f.Sources)
	}
}

// --- FetchAndSummarize ---------------------------------------------------------

// TestFetchAndSummarize_ReadsAndCites proves the direct read-one-page path: it
// fetches a page, summarizes it via the mock backend, and cites the single URL.
func TestFetchAndSummarize_ReadsAndCites(t *testing.T) {
	srv := htmlServer(t, "<title>10-K</title><p>The company reported record cash flow.</p>", nil)
	defer srv.Close()

	c := NewClient(WithHTTPClient(srv.Client()))
	be := &scriptBackend{name: "mock", reply: "Record cash flow reported."}

	f, err := FetchAndSummarize(context.Background(), c, []analyst.Backend{be}, srv.URL, "plain English please")
	if err != nil {
		t.Fatalf("FetchAndSummarize: %v", err)
	}
	if strings.TrimSpace(f.Brief) == "" {
		t.Error("expected a non-empty brief")
	}
	if len(f.Sources) != 1 || f.Sources[0] != srv.URL {
		t.Fatalf("expected the one URL cited, got %v", f.Sources)
	}
	if !strings.Contains(be.lastPrompt, "record cash flow") {
		t.Errorf("summarizer did not see the page text:\n%s", be.lastPrompt)
	}
}

// TestFetchAndSummarize_DeadURL_NoFabrication proves a failed fetch yields an honest
// no-source brief and never calls the model.
func TestFetchAndSummarize_DeadURL_NoFabrication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(WithHTTPClient(srv.Client()))
	be := &scriptBackend{name: "mock", reply: "FABRICATED"}

	f, err := FetchAndSummarize(context.Background(), c, []analyst.Backend{be}, srv.URL, "")
	if err != nil {
		t.Fatalf("FetchAndSummarize: %v", err)
	}
	if f.HasSources() || strings.Contains(f.Brief, "FABRICATED") {
		t.Fatalf("dead URL must yield no source and no fabricated brief; got brief=%q sources=%v", f.Brief, f.Sources)
	}
	if be.lastPrompt != "" {
		t.Error("the model must NOT be called when the page could not be read")
	}
}

// --- search provider -----------------------------------------------------------

// TestDuckDuckGoSearch_ParsesResultsReadOnly proves the keyless provider parses
// result links from the DDG HTML-lite markup and unwraps the redirect — driven
// against an httptest server (NO live DDG).
func TestDuckDuckGoSearch_ParsesResultsReadOnly(t *testing.T) {
	// The DDG lite page wraps targets in /l/?uddg=<encoded>.
	body := `<html><body>
		<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fnews">Example News</a>
		<a class="result__a" href="https://plain.example.org/article">Plain Link</a>
		<a class="something-else" href="https://ignored.example/ad">Ad</a>
	</body></html>`
	var nonRead int
	srv := htmlServer(t, body, func(method string) {
		if method != http.MethodGet && method != http.MethodHead {
			nonRead++
		}
	})
	defer srv.Close()

	// Point the provider's client base at the test server by overriding the endpoint
	// through fetchRaw indirectly: we use a Client whose http hits the test server.
	c := NewClient(WithHTTPClient(srv.Client()))
	d := ddgAgainst{c: c, endpoint: srv.URL}
	results, err := d.Search(context.Background(), "example")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if nonRead != 0 {
		t.Fatalf("search issued %d non-read methods", nonRead)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 parsed results, got %d: %+v", len(results), results)
	}
	if results[0].URL != "https://example.com/news" {
		t.Errorf("redirect not unwrapped: %q", results[0].URL)
	}
	if results[1].URL != "https://plain.example.org/article" {
		t.Errorf("plain link not parsed: %q", results[1].URL)
	}
}

// TestParseDDGResults_NoMatch_Honest proves that markup with no result anchors yields
// zero results (so the real Search reports "no parseable results" honestly).
func TestParseDDGResults_NoMatch_Honest(t *testing.T) {
	if got := parseDDGResults("<html><body>nothing here</body></html>"); len(got) != 0 {
		t.Fatalf("expected zero results, got %v", got)
	}
}

// --- helpers -------------------------------------------------------------------

// ddgAgainst is a tiny test shim that runs the DuckDuckGoSearch parsing logic
// against an arbitrary endpoint (the httptest server) instead of the live DDG host,
// reusing the exact production fetchRaw + parseDDGResults path.
type ddgAgainst struct {
	c        *Client
	endpoint string
}

func (d ddgAgainst) Search(ctx context.Context, query string) ([]Result, error) {
	raw, err := d.c.fetchRaw(ctx, d.endpoint+"?q="+query)
	if err != nil {
		return nil, err
	}
	results := parseDDGResults(raw)
	if len(results) == 0 {
		return nil, errors.New("no parseable results")
	}
	return results, nil
}

// htmlServer starts an httptest server serving the given HTML. The optional onReq
// callback receives each request's method (so a test can assert read-only).
func htmlServer(t *testing.T, html string, onReq func(method string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onReq != nil {
			onReq(r.Method)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
}

// sharedClient returns an *http.Client that can reach BOTH test servers. httptest
// servers share the default transport, so either server's client works; we return
// the first server's client for clarity.
func sharedClient(a, _ *httptest.Server) *http.Client {
	return a.Client()
}
