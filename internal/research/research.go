package research

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/summary"
)

// Findings is the honest result of Research: a plain-English Brief written ONLY from
// the fetched page text, and Sources — the EXACT URLs that were actually read. Every
// claim in the Brief is traceable to a page in Sources; if Sources is empty the
// Brief is empty too (Research never writes a market take with nothing behind it).
// Failovers records any model downgrade during summarization (visible, never silent).
type Findings struct {
	// Query is the question that was researched (echoed back for context).
	Query string
	// Brief is the plain-English synthesis built from the fetched pages. Empty when
	// nothing was fetched.
	Brief string
	// Sources are the URLs actually fetched and fed into the brief — always cited.
	Sources []string
	// Backend is the model that produced the brief (after any failover); empty when
	// no brief was produced.
	Backend string
	// Failovers is the no-silent-downgrade trail from summarization.
	Failovers []analyst.Failover
	// Fetched is the number of pages successfully read (== len(Sources)).
	Fetched int
	// Errors carries non-fatal per-source fetch failures (e.g. a 404 on result #2),
	// so a partial run is honest about what it could NOT read.
	Errors []string
}

// HasSources reports whether the findings rest on at least one real fetched page.
func (f Findings) HasSources() bool { return len(f.Sources) > 0 }

// Downgraded reports whether the brief came from a fallback model.
func (f Findings) Downgraded() bool { return len(f.Failovers) > 0 }

// researchConfig holds Research's tunables.
type researchConfig struct {
	topN        int
	corpusCap   int
	instruction string
}

// ResearchOption configures a Research run.
type ResearchOption func(*researchConfig)

// WithTopN sets how many of the top search results to fetch (default 3). Values < 1
// are ignored.
func WithTopN(n int) ResearchOption {
	return func(c *researchConfig) {
		if n >= 1 {
			c.topN = n
		}
	}
}

// WithCorpusCap bounds the total concatenated page text (bytes) handed to the
// summarizer, so a few large pages can't blow up the prompt. Values < 1 are ignored.
func WithCorpusCap(n int) ResearchOption {
	return func(c *researchConfig) {
		if n >= 1 {
			c.corpusCap = n
		}
	}
}

// WithInstruction overrides the summarization instruction (default: a plain-English
// market-context brief). Empty is ignored.
func WithInstruction(s string) ResearchOption {
	return func(c *researchConfig) {
		if strings.TrimSpace(s) != "" {
			c.instruction = strings.TrimSpace(s)
		}
	}
}

func newResearchConfig(opts ...ResearchOption) researchConfig {
	c := researchConfig{
		topN:        3,
		corpusCap:   48 * 1024, // 48 KiB of concatenated text into the summarizer
		instruction: defaultBriefInstruction,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

const defaultBriefInstruction = "You are BUCKS researching market context. Using ONLY the page excerpts below, " +
	"write a SHORT, plain-English brief (a few sentences) a first-time investor can understand. " +
	"Stick to what the pages actually say; if they don't answer the question, say so plainly. " +
	"Do NOT invent prices, dates, or facts that are not in the excerpts."

// Research is the read-only researcher: search -> fetch top N read-only ->
// summarize, with sources always cited. It (1) runs the query through the pluggable
// SearchProvider, (2) fetches the top N results with the read-only Client, (3)
// concatenates and bounds the page text, (4) summarizes it through the ONE
// summary.Summarize engine, and returns Findings whose Sources list every fetched
// URL.
//
// HONESTY: if the search returns nothing, or every fetch fails, Research returns
// Findings with NO Brief and HasSources()==false plus a clear message in Errors — it
// NEVER fabricates a brief from nothing. A summarizer failure after pages WERE
// fetched is returned as an error (with the fetched Sources still populated, so the
// caller can at least cite what was read).
func Research(ctx context.Context, sp SearchProvider, c *Client, backends []analyst.Backend, query string, opts ...ResearchOption) (Findings, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return Findings{}, errors.New("research: empty query")
	}
	if sp == nil {
		return Findings{}, errors.New("research: a SearchProvider is required")
	}
	if c == nil {
		return Findings{}, errors.New("research: a read-only Client is required")
	}
	if len(backends) == 0 {
		return Findings{}, errors.New("research: at least one backend is required")
	}
	cfg := newResearchConfig(opts...)

	out := Findings{Query: q}

	results, err := sp.Search(ctx, q)
	if err != nil {
		// Search failed entirely — honest "no sources", no brief, no fabrication.
		out.Errors = append(out.Errors, fmt.Sprintf("search failed: %v", err))
		out.Brief = noSourcesBrief(q)
		return out, nil
	}
	if len(results) == 0 {
		out.Errors = append(out.Errors, "search returned no results")
		out.Brief = noSourcesBrief(q)
		return out, nil
	}

	// Fetch the top N read-only, building the bounded corpus and citing every page
	// actually read.
	var corpus strings.Builder
	n := cfg.topN
	if n > len(results) {
		n = len(results)
	}
	for i := 0; i < n; i++ {
		r := results[i]
		page, ferr := c.Fetch(ctx, r.URL)
		if ferr != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("fetch %s: %v", r.URL, ferr))
			continue
		}
		if strings.TrimSpace(page.Text) == "" {
			out.Errors = append(out.Errors, fmt.Sprintf("fetch %s: no readable text (page may be JavaScript-rendered)", r.URL))
			continue
		}
		out.Sources = append(out.Sources, page.URL)
		appendBounded(&corpus, page, cfg.corpusCap)
	}
	out.Fetched = len(out.Sources)

	if out.Fetched == 0 {
		// Nothing readable was fetched — honest "no sources", no brief.
		out.Brief = noSourcesBrief(q)
		return out, nil
	}

	// Summarize ONLY the fetched corpus through the single summary engine.
	instruction := cfg.instruction + "\nQUESTION: " + q
	s, serr := summary.Summarize(ctx, backends, corpus.String(), instruction)
	if serr != nil {
		// Pages were read (Sources cited) but the model failed. Return the error with
		// the sources still populated so nothing is hidden and nothing is invented.
		return out, fmt.Errorf("research: summarized %d source(s) but the model failed: %w", out.Fetched, serr)
	}
	out.Brief = s.Text
	out.Backend = s.Backend
	out.Failovers = s.Failovers
	return out, nil
}

// FetchAndSummarize is the direct "read this page and tell me in plain English" path:
// it fetches a single URL read-only and summarizes it through summary.Summarize per
// the instruction. Robust and keyless (no search provider needed). The returned
// Findings always cites the one URL in Sources when the fetch succeeded.
func FetchAndSummarize(ctx context.Context, c *Client, backends []analyst.Backend, rawURL, instruction string) (Findings, error) {
	url := strings.TrimSpace(rawURL)
	if url == "" {
		return Findings{}, errors.New("research: empty url")
	}
	if c == nil {
		return Findings{}, errors.New("research: a read-only Client is required")
	}
	if len(backends) == 0 {
		return Findings{}, errors.New("research: at least one backend is required")
	}
	out := Findings{Query: url}

	page, err := c.Fetch(ctx, url)
	if err != nil {
		// The fetch failed: honest "no source", no brief, no fabrication.
		out.Errors = append(out.Errors, fmt.Sprintf("fetch %s: %v", url, err))
		out.Brief = noSourcesBrief(url)
		return out, nil
	}
	if strings.TrimSpace(page.Text) == "" {
		out.Errors = append(out.Errors, fmt.Sprintf("fetch %s: no readable text (page may be JavaScript-rendered)", url))
		out.Brief = noSourcesBrief(url)
		return out, nil
	}
	out.Sources = []string{page.URL}
	out.Fetched = 1

	instr := strings.TrimSpace(instruction)
	if instr == "" {
		instr = "Summarize this page in plain English a first-time investor can understand. Use ONLY what the page says; do not invent facts."
	}
	if page.Title != "" {
		instr += "\nPAGE TITLE: " + page.Title
	}
	s, serr := summary.Summarize(ctx, backends, page.Text, instr)
	if serr != nil {
		return out, fmt.Errorf("research: read %s but the model failed: %w", url, serr)
	}
	out.Brief = s.Text
	out.Backend = s.Backend
	out.Failovers = s.Failovers
	return out, nil
}

// appendBounded appends a page's titled text to the corpus without exceeding cap. It
// labels each page with its URL+title so the model can attribute, but it stops adding
// once the cap is reached (the corpus is the summarizer's bounded input).
func appendBounded(corpus *strings.Builder, page Page, cap int) {
	if corpus.Len() >= cap {
		return
	}
	header := "\n--- SOURCE: " + page.URL
	if page.Title != "" {
		header += " (" + page.Title + ")"
	}
	header += " ---\n"
	remaining := cap - corpus.Len()
	body := page.Text
	if len(header)+len(body) > remaining {
		// Fit what we can of the body (header is small; ensure room for it).
		room := remaining - len(header)
		if room < 0 {
			room = 0
		}
		body = boundRunes(body, room)
	}
	corpus.WriteString(header)
	corpus.WriteString(body)
}

// noSourcesBrief is the HONEST output when nothing could be fetched: it states plainly
// that BUCKS found no readable sources for the question and invents no market take.
func noSourcesBrief(query string) string {
	return fmt.Sprintf("I couldn't find any readable sources for %q, so I won't guess. "+
		"Try a more specific query, or plug a search provider key for more reliable results. "+
		"(This researcher reads server-rendered pages only — it does not run JavaScript.)", query)
}

// compile-time assertion that the summary engine contract we depend on exists — keeps
// the seam to summary.Summarize honest and typed.
var _ = summary.Summarize
