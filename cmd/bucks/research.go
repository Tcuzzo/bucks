package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/research"
)

// runResearchStdio uses the shared saved-config-plus-environment backend resolver.
func runResearchStdio(configPath, query string) error {
	return runResearch(os.Stdout, runtimeResearchBackends(configPath), defaultSearchProvider, research.NewClient(), query)
}

// runReadStdio is the production `bucks read <url>` entry point: the direct
// "read this page and tell me in plain English" path. Robust and keyless (no search
// needed) — it fetches one URL read-only and summarizes it, citing the URL.
func runReadStdio(configPath, url string) error {
	return runRead(os.Stdout, runtimeResearchBackends(configPath), research.NewClient(), url)
}

func runtimeResearchBackends(configPath string) backendsFactory {
	return func() ([]analyst.Backend, error) {
		return runtimeChatBackends(configPath, passphraseFromEnv())
	}
}

// envResearchBackends shares the SINGLE backend-selection seam (envChatBackend) with
// chat and summary, so a BUCKS_CHAT_PROVIDER choice applies identically here. Returns
// (nil, nil) when nothing is configured (the clean no-backend case).
func envResearchBackends() ([]analyst.Backend, error) {
	backend, err := envChatBackend()
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, nil
	}
	return []analyst.Backend{backend}, nil
}

// searchProviderFactory builds the SearchProvider a research run uses, given the
// read-only client (the keyless default needs it). It is an injection seam so the
// default suite drives the command with a mock provider (no network).
type searchProviderFactory func(c *research.Client) research.SearchProvider

// defaultSearchProvider is the production search provider: the keyless, best-effort
// DuckDuckGo HTML-lite scraper over the read-only client. It is explicitly flaky (see
// its doc) — a user wanting reliable search plugs a keyed provider via the interface.
func defaultSearchProvider(c *research.Client) research.SearchProvider {
	return research.DuckDuckGoSearch{Client: c, MaxResults: 5}
}

// runResearch runs a web-research query and prints the brief + cited sources using the
// injected backends + search provider + read-only client. With NO backend it prints a
// clear message and exits 0. A network/search failure becomes a clear message and an
// honest "no sources" brief — never a crash, never a fabricated take. out + factories
// are injected so the default suite drives the real entry point offline.
func runResearch(out io.Writer, newBackends backendsFactory, newProvider searchProviderFactory, c *research.Client, query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		fmt.Fprintln(out, "bucks research: nothing to research — usage: bucks research \"<your question>\"")
		return nil
	}
	backends, err := newBackends()
	if err != nil {
		return fmt.Errorf("research: %w", err)
	}
	if len(backends) == 0 {
		fmt.Fprint(out, noBackendMessage("research"))
		return nil
	}

	sp := newProvider(c)
	f, err := research.Research(context.Background(), sp, c, backends, q)
	if err != nil {
		// A summarizer failure AFTER pages were read: report it, but still cite what
		// was read (nothing hidden).
		fmt.Fprintf(out, "bucks research: %v\n", err)
		printSources(out, f.Sources)
		return nil
	}
	printFindings(out, f)
	return nil
}

// runRead fetches one URL read-only and prints a plain-English brief + the cited URL.
func runRead(out io.Writer, newBackends backendsFactory, c *research.Client, url string) error {
	u := strings.TrimSpace(url)
	if u == "" {
		fmt.Fprintln(out, "bucks read: no URL given — usage: bucks read <url>")
		return nil
	}
	backends, err := newBackends()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(backends) == 0 {
		fmt.Fprint(out, noBackendMessage("read"))
		return nil
	}

	f, err := research.FetchAndSummarize(context.Background(), c, backends, u, "")
	if err != nil {
		fmt.Fprintf(out, "bucks read: %v\n", err)
		printSources(out, f.Sources)
		return nil
	}
	printFindings(out, f)
	return nil
}

// printFindings writes the brief, then the cited sources (always), then any non-fatal
// per-source errors and a downgrade note — so the owner sees exactly what BUCKS read
// and where any number could (or could not) be backed.
func printFindings(out io.Writer, f research.Findings) {
	fmt.Fprintln(out, strings.TrimSpace(f.Brief))
	printSources(out, f.Sources)
	if f.Downgraded() {
		fmt.Fprintf(out, "(note: brief written on fallback model %q after %d failover(s))\n",
			f.Backend, len(f.Failovers))
	}
	for _, e := range f.Errors {
		fmt.Fprintf(out, "(could not read: %s)\n", e)
	}
}

// printSources prints the cited source URLs (or an honest "no sources read" line). The
// sources are ALWAYS shown so every brief is traceable to the pages it rests on.
func printSources(out io.Writer, sources []string) {
	if len(sources) == 0 {
		fmt.Fprintln(out, "Sources: none read.")
		return
	}
	fmt.Fprintln(out, "Sources:")
	for _, s := range sources {
		fmt.Fprintf(out, "  - %s\n", s)
	}
}
