package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestObsidianTradeSymbolEdge proves the trade note links to its [[SYMBOL]] and
// [[setup]] notes (the trade->symbol KG edge), and that the symbol/setup notes
// accrue the backlink to the trade.
func TestObsidianTradeSymbolEdge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}

	tr := TradeMemory{
		ID: 1, Symbol: "AAPL", Setup: "breakout",
		Entry: mustDecimal(t, "187.42"), Exit: mustDecimal(t, "191.10"), PnL: mustDecimal(t, "3.68"),
		Lesson: "held the chop", TS: time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
	}
	if err := v.WriteTrade(tr); err != nil {
		t.Fatalf("write trade: %v", err)
	}

	// The trade note's outgoing links must include the symbol and setup notes.
	links, err := v.LinksFrom("trade-1")
	if err != nil {
		t.Fatalf("links from trade note: %v", err)
	}
	if !contains(links, "AAPL") {
		t.Fatalf("trade->symbol edge missing; trade-1 links: %v", links)
	}
	if !contains(links, "breakout") {
		t.Fatalf("trade->setup edge missing; trade-1 links: %v", links)
	}

	// The symbol note must have a backlink to the trade note.
	symLinks, err := v.LinksFrom("AAPL")
	if err != nil {
		t.Fatalf("links from symbol note: %v", err)
	}
	if !contains(symLinks, "trade-1") {
		t.Fatalf("symbol backlink missing; AAPL links: %v", symLinks)
	}

	// And the whole graph, read back, contains the directed edge trade-1 -> AAPL.
	g, err := v.ReadGraph()
	if err != nil {
		t.Fatalf("read graph: %v", err)
	}
	if !contains(g["trade-1"], "AAPL") {
		t.Fatalf("graph edge trade-1 -> AAPL absent; graph: %v", g)
	}
	if !contains(g["AAPL"], "trade-1") {
		t.Fatalf("graph backlink AAPL -> trade-1 absent; graph: %v", g)
	}

	// Money is rendered exactly (no float) in the note text.
	data, err := os.ReadFile(filepath.Join(dir, "trade-1.md"))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if !containsSub(string(data), "entry:: 187.42") {
		t.Fatalf("exact decimal entry missing from note:\n%s", data)
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// TestSymbolNoteAccruesMultipleBacklinks proves a [[SYMBOL]] note grows backlinks
// from several trades (the KG connecting many memories), idempotently.
func TestSymbolNoteAccruesMultipleBacklinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	for id := int64(1); id <= 3; id++ {
		tr := TradeMemory{
			ID: id, Symbol: "NVDA", Setup: "momentum",
			Entry: mustDecimal(t, "900"), Exit: mustDecimal(t, "910"), PnL: mustDecimal(t, "10"),
		}
		if err := v.WriteTrade(tr); err != nil {
			t.Fatalf("write trade %d: %v", id, err)
		}
	}
	// Re-write trade 1 to prove idempotency (no duplicate backlink).
	if err := v.WriteTrade(TradeMemory{
		ID: 1, Symbol: "NVDA", Setup: "momentum",
		Entry: mustDecimal(t, "900"), Exit: mustDecimal(t, "910"), PnL: mustDecimal(t, "10"),
	}); err != nil {
		t.Fatalf("rewrite trade 1: %v", err)
	}

	links, err := v.LinksFrom("NVDA")
	if err != nil {
		t.Fatalf("links from NVDA: %v", err)
	}
	for _, want := range []string{"trade-1", "trade-2", "trade-3"} {
		if !contains(links, want) {
			t.Fatalf("NVDA missing backlink %s; links: %v", want, links)
		}
	}
	// parseLinks dedupes, so exactly 3 distinct trade backlinks.
	count := 0
	for _, l := range links {
		if len(l) >= 6 && l[:6] == "trade-" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("expected 3 distinct trade backlinks (idempotent), got %d: %v", count, links)
	}
}

// TestVaultConcurrentWritesSameSymbol proves Vault writes are goroutine-safe: N
// goroutines write distinct trades that all share ONE symbol, racing on that single
// symbol note's read-modify-write. Without the Vault mutex this loses updates (the
// symbol note ends with fewer than N backlinks) and trips the race detector. With
// it, the symbol note has exactly N distinct trade backlinks and the graph is
// consistent. Run under -race to catch the data race directly.
func TestVaultConcurrentWritesSameSymbol(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}

	const n = 20
	const symbol = "NVDA"

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for id := int64(1); id <= n; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			tr := TradeMemory{
				ID: id, Symbol: symbol, Setup: "momentum",
				Entry: mustDecimal(t, "900"), Exit: mustDecimal(t, "910"), PnL: mustDecimal(t, "10"),
			}
			if err := v.WriteTrade(tr); err != nil {
				errs <- err
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent WriteTrade: %v", err)
	}

	// The symbol note must hold exactly N distinct trade backlinks — no lost updates,
	// no duplicates. parseLinks dedupes, so a duplicate would show as a missing id,
	// and a lost update (clobbered write) drops an id; either way the count != N.
	links, err := v.LinksFrom(symbol)
	if err != nil {
		t.Fatalf("links from %s: %v", symbol, err)
	}
	seen := make(map[string]bool, n)
	for _, l := range links {
		if strings.HasPrefix(l, "trade-") {
			seen[l] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct trade backlinks on %s, got %d (lost update / race): %v",
			n, symbol, len(seen), links)
	}
	for id := int64(1); id <= n; id++ {
		want := tradeNoteTitle(TradeMemory{ID: id})
		if !seen[want] {
			t.Fatalf("backlink %s missing from %s (lost update); have: %v", want, symbol, links)
		}
	}

	// The graph read back is consistent: every trade note links to the symbol, and
	// the symbol note links back to every trade note.
	g, err := v.ReadGraph()
	if err != nil {
		t.Fatalf("read graph: %v", err)
	}
	for id := int64(1); id <= n; id++ {
		tradeNote := tradeNoteTitle(TradeMemory{ID: id})
		if !contains(g[tradeNote], symbol) {
			t.Fatalf("graph edge %s -> %s missing; graph: %v", tradeNote, symbol, g)
		}
		if !contains(g[symbol], tradeNote) {
			t.Fatalf("graph backlink %s -> %s missing; graph: %v", symbol, tradeNote, g)
		}
	}
}

func TestMarketNoteLinksSymbol(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	if err := v.WriteMarket(MarketMemory{ID: 7, Symbol: "BTC-USD", Observation: "funding negative"}); err != nil {
		t.Fatalf("write market: %v", err)
	}
	links, err := v.LinksFrom("market-7")
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	if !contains(links, "BTC-USD") {
		t.Fatalf("market->symbol edge missing: %v", links)
	}
	symLinks, err := v.LinksFrom("BTC-USD")
	if err != nil {
		t.Fatalf("symbol links: %v", err)
	}
	if !contains(symLinks, "market-7") {
		t.Fatalf("symbol backlink to market missing: %v", symLinks)
	}
}

// TestZipRoundTrip proves the store file + vault dir survive zip -> ship -> unpack:
// write memories, copy both to a new path (simulating unzip), reopen, and assert
// full recall + the KG intact.
func TestZipRoundTrip(t *testing.T) {
	base := t.TempDir()
	origDB := filepath.Join(base, "orig", "memory.sqlite")
	origVault := filepath.Join(base, "orig", "vault")
	if err := os.MkdirAll(filepath.Dir(origDB), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m, err := New(origDB, origVault)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	saved, err := m.RememberTrade(TradeMemory{
		Symbol: "AAPL", Setup: "breakout",
		Entry: mustDecimal(t, "187.42"), Exit: mustDecimal(t, "191.10"), PnL: mustDecimal(t, "3.68"),
		Lesson: "survives the zip",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := m.RememberMarket(MarketMemory{Symbol: "AAPL", Observation: "earnings beat"}); err != nil {
		t.Fatalf("remember market: %v", err)
	}
	// Close so the SQLite file (incl. WAL) is fully flushed before copy.
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate zip -> unpack: copy the DB file (+ any WAL/SHM siblings) and the vault
	// to a brand-new path.
	shipDB := filepath.Join(base, "shipped", "memory.sqlite")
	shipVault := filepath.Join(base, "shipped", "vault")
	if err := os.MkdirAll(filepath.Dir(shipDB), 0o755); err != nil {
		t.Fatalf("mkdir ship: %v", err)
	}
	copyFile(t, origDB, shipDB)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(origDB + suffix); err == nil {
			copyFile(t, origDB+suffix, shipDB+suffix)
		}
	}
	copyDir(t, origVault, shipVault)

	// Reopen at the shipped path and assert full recall.
	m2, err := New(shipDB, shipVault)
	if err != nil {
		t.Fatalf("reopen shipped: %v", err)
	}
	defer m2.Close()

	trades, err := m2.Store.RecallTrades(TradeFilter{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("recall after unpack: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade after unpack, got %d", len(trades))
	}
	if trades[0].ID != saved.ID || trades[0].PnL.String() != "3.68" {
		t.Fatalf("trade did not survive round trip exactly: %+v", trades[0])
	}
	markets, err := m2.Store.RecallMarket(MarketFilter{Symbol: "AAPL"})
	if err != nil || len(markets) != 1 {
		t.Fatalf("market did not survive: %v (n=%d)", err, len(markets))
	}

	// KG intact in the shipped vault: trade->symbol edge + symbol backlink.
	g, err := m2.Vault.ReadGraph()
	if err != nil {
		t.Fatalf("read shipped graph: %v", err)
	}
	tradeNote := tradeNoteTitle(saved)
	if !contains(g[tradeNote], "AAPL") {
		t.Fatalf("KG edge %s -> AAPL lost in round trip; graph: %v", tradeNote, g)
	}
	if !contains(g["AAPL"], tradeNote) {
		t.Fatalf("KG backlink AAPL -> %s lost in round trip; graph: %v", tradeNote, g)
	}
}
