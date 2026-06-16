package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"bucks/internal/orders"
)

// Vault is an Obsidian-compatible markdown vault: BUCKS's memory written as linked
// notes so the owner can open it as a knowledge graph in Obsidian. The wikilink
// graph IS the knowledge graph — no vector DB in v1.
//
// Each remembered trade becomes a trade note that links to its [[SYMBOL]] note and
// its [[setup-name]] note; the symbol/setup notes accrue backlinks. Reading the
// vault back and parsing the [[targets]] reconstructs the link graph, so a test can
// assert the trade -> symbol edge exists and the symbol note got the backlink.
type Vault struct {
	dir string
	// mu serializes the non-atomic read-modify-write of note files. Two concurrent
	// WriteTrade calls for the same symbol both read the symbol note, append their
	// backlink, and write it back; without this lock the later write clobbers the
	// earlier one (a lost update). Holding mu around every vault file mutation
	// (writeNote + appendBacklink) makes Vault safe for concurrent use.
	mu sync.Mutex
}

// wikilinkRe extracts the inner target of each [[wikilink]] in note text. Targets
// are matched non-greedily and may not contain ']' or '[' (standard Obsidian).
var wikilinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// OpenVault opens (creating if needed) an Obsidian vault directory at dir.
func OpenVault(dir string) (*Vault, error) {
	if dir == "" {
		return nil, fmt.Errorf("memory: empty vault dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: open vault %q: %w", dir, err)
	}
	return &Vault{dir: dir}, nil
}

// Dir returns the vault's root directory.
func (v *Vault) Dir() string { return v.dir }

// safeName turns an arbitrary note title into a filesystem-safe markdown filename
// stem. Obsidian resolves [[Foo]] case-and-path-insensitively to Foo.md, so we keep
// the visible title verbatim inside the file and only sanitize the filename.
func safeName(title string) string {
	repl := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", "\"", "-", "<", "-", ">", "-", "|", "-",
	)
	s := strings.TrimSpace(repl.Replace(title))
	if s == "" {
		s = "untitled"
	}
	return s
}

// notePath is the on-disk path for a note titled title.
func (v *Vault) notePath(title string) string {
	return filepath.Join(v.dir, safeName(title)+".md")
}

// tradeNoteTitle is the deterministic title for a trade note.
func tradeNoteTitle(t TradeMemory) string {
	return fmt.Sprintf("trade-%d", t.ID)
}

// WriteTrade writes (or rewrites) the markdown note for a trade and ensures its
// linked [[SYMBOL]] and [[setup]] notes exist with a backlink to this trade. Money
// is rendered from the exact Decimal string, never a float. After this returns, the
// trade->symbol and trade->setup edges are present in the vault's link graph.
func (v *Vault) WriteTrade(t TradeMemory) error {
	if t.ID == 0 {
		return fmt.Errorf("memory: trade note needs a persisted ID (got 0)")
	}
	symbol := strings.TrimSpace(t.Symbol)
	if symbol == "" {
		return fmt.Errorf("memory: trade note needs a symbol")
	}
	setup := strings.TrimSpace(t.Setup)

	title := tradeNoteTitle(t)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- symbol:: [[%s]]\n", symbol)
	if setup != "" {
		fmt.Fprintf(&b, "- setup:: [[%s]]\n", setup)
	}
	fmt.Fprintf(&b, "- entry:: %s\n", t.Entry.String())
	fmt.Fprintf(&b, "- exit:: %s\n", t.Exit.String())
	fmt.Fprintf(&b, "- pnl:: %s\n", t.PnL.String())
	fmt.Fprintf(&b, "- ts:: %s\n", t.TS.UTC().Format(time.RFC3339Nano))
	if t.Lesson != "" {
		fmt.Fprintf(&b, "\n## lesson\n%s\n", t.Lesson)
	}
	if err := v.writeNote(title, b.String()); err != nil {
		return err
	}

	// Ensure the symbol note exists and accrues a backlink to this trade.
	if err := v.appendBacklink(symbol, "## trades\n", title); err != nil {
		return err
	}
	if setup != "" {
		if err := v.appendBacklink(setup, "## trades\n", title); err != nil {
			return err
		}
	}
	return nil
}

// WriteMarket writes a market observation note linked to its [[SYMBOL]] and
// backlinks the symbol note.
func (v *Vault) WriteMarket(m MarketMemory) error {
	if m.ID == 0 {
		return fmt.Errorf("memory: market note needs a persisted ID (got 0)")
	}
	symbol := strings.TrimSpace(m.Symbol)
	if symbol == "" {
		return fmt.Errorf("memory: market note needs a symbol")
	}
	title := fmt.Sprintf("market-%d", m.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- symbol:: [[%s]]\n", symbol)
	fmt.Fprintf(&b, "- ts:: %s\n", m.TS.UTC().Format(time.RFC3339Nano))
	if m.Observation != "" {
		fmt.Fprintf(&b, "\n## observation\n%s\n", m.Observation)
	}
	if err := v.writeNote(title, b.String()); err != nil {
		return err
	}
	return v.appendBacklink(symbol, "## observations\n", title)
}

// writeNote writes content to the note titled title (overwriting), committed to
// disk via os.WriteFile (a single durable write). It holds v.mu so a concurrent
// same-title rewrite can't interleave with a read-modify-write in appendBacklink.
func (v *Vault) writeNote(title, content string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.writeNoteLocked(title, content)
}

// writeNoteLocked is the unlocked core of writeNote. The caller MUST already hold
// v.mu (this keeps the lock non-reentrant: appendBacklink locks once and calls
// this, rather than writeNote re-locking the same mutex and deadlocking).
func (v *Vault) writeNoteLocked(title, content string) error {
	if err := os.WriteFile(v.notePath(title), []byte(content), 0o644); err != nil {
		return fmt.Errorf("memory: write note %q: %w", title, err)
	}
	return nil
}

// appendBacklink ensures the note titled title exists and contains a [[child]]
// backlink under the given section header. It is idempotent: an already-present
// backlink is not duplicated. This is how a symbol/setup note accrues its graph
// edges from many trades.
//
// The read (os.ReadFile) and the write-back (writeNoteLocked) are held under v.mu
// as one atomic critical section, so two concurrent WriteTrade calls for the same
// symbol can't both read the old note and clobber each other's backlink (lost
// update). This is what makes Vault goroutine-safe.
func (v *Vault) appendBacklink(title, section, child string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	path := v.notePath(title)
	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("memory: read note %q: %w", title, err)
		}
		existing = []byte(fmt.Sprintf("# %s\n\n", title))
	}
	link := fmt.Sprintf("- [[%s]]\n", child)
	text := string(existing)
	if strings.Contains(text, "[["+child+"]]") {
		return nil // edge already recorded; stay idempotent
	}
	if !strings.Contains(text, section) {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + section
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += link
	return v.writeNoteLocked(title, text)
}

// LinkGraph is the parsed wikilink graph of a vault: for each note title, the set
// of note titles it links to via [[wikilinks]]. This is the knowledge graph.
type LinkGraph map[string][]string

// ReadGraph parses every .md note in the vault and returns the directed wikilink
// graph (note -> [linked targets]). Targets are returned sorted+deduped so tests
// are deterministic. An edge A -> B means note A contains [[B]].
func (v *Vault) ReadGraph() (LinkGraph, error) {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return nil, fmt.Errorf("memory: read vault dir: %w", err)
	}
	g := make(LinkGraph)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		title := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(v.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory: read note %q: %w", e.Name(), err)
		}
		g[title] = parseLinks(string(data))
	}
	return g, nil
}

// LinksFrom returns the sorted, deduped link targets of a single note by title.
func (v *Vault) LinksFrom(title string) ([]string, error) {
	data, err := os.ReadFile(v.notePath(title))
	if err != nil {
		return nil, fmt.Errorf("memory: read note %q: %w", title, err)
	}
	return parseLinks(string(data)), nil
}

// parseLinks pulls every [[target]] out of note text, returning a sorted, deduped
// slice. A note with no links yields a non-nil empty slice for stable comparisons.
func parseLinks(text string) []string {
	matches := wikilinkRe.FindAllStringSubmatch(text, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		target := strings.TrimSpace(m[1])
		if target == "" {
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// Memory ties the SQLite store and the Obsidian vault together: a single facade
// that records a memory to BOTH the durable store AND the human-browsable KG, and
// keeps Forget honest across both. Money stays Decimal end-to-end.
type Memory struct {
	Store *Store
	Vault *Vault
}

// New opens a combined memory: SQLite at dbPath and an Obsidian vault at vaultDir.
func New(dbPath, vaultDir string) (*Memory, error) {
	st, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	vt, err := OpenVault(vaultDir)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return &Memory{Store: st, Vault: vt}, nil
}

// Close releases the store handle.
func (m *Memory) Close() error {
	if m == nil || m.Store == nil {
		return nil
	}
	return m.Store.Close()
}

// RememberTrade persists a trade to SQLite and mirrors it into the vault KG.
func (m *Memory) RememberTrade(t TradeMemory) (TradeMemory, error) {
	saved, err := m.Store.RememberTrade(t)
	if err != nil {
		return TradeMemory{}, err
	}
	if err := m.Vault.WriteTrade(saved); err != nil {
		return TradeMemory{}, err
	}
	return saved, nil
}

// RememberMarket persists a market observation and mirrors it into the vault KG.
func (m *Memory) RememberMarket(mm MarketMemory) (MarketMemory, error) {
	saved, err := m.Store.RememberMarket(mm)
	if err != nil {
		return MarketMemory{}, err
	}
	if err := m.Vault.WriteMarket(saved); err != nil {
		return MarketMemory{}, err
	}
	return saved, nil
}

// MoneyEqual reports whether two Decimals are numerically equal — a small helper so
// callers/tests compare money by value (not float) without importing the decimal
// package directly.
func MoneyEqual(a, b orders.Decimal) bool { return a.Equal(b) }
