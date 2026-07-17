package understanding

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moneyPathPackages are the packages that decide, size, risk, or place REAL trades.
// An LLM belongs in none of them.
var moneyPathPackages = []string{
	"internal/orders",
	"internal/risk",
	"internal/strategy",
	"internal/brokers",
	"internal/kernel",
	"internal/ledger",
	"internal/backtest",
}

// TestUnderstandingIsNeverInTheMoneyPath is a STRUCTURAL guard, not a comment: it
// parses the real import graph and fails if any order/risk/execution package ever
// imports this organ.
//
// Understanding grades BUILDS. It is a language model's opinion, and a language
// model's opinion must never size a position, pass a risk check, or place an order.
// The organ being advisory is enforced here rather than merely promised — the next
// person to wire it somewhere it does not belong gets a red test, not a code review
// they might not receive.
func TestUnderstandingIsNeverInTheMoneyPath(t *testing.T) {
	root := repoRoot(t)
	const organ = `"bucks/internal/understanding"`

	checked := 0
	for _, pkg := range moneyPathPackages {
		dir := filepath.Join(root, pkg)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("money-path package %s not found — this guard is stale and must be updated, not deleted: %v", pkg, err)
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if perr != nil {
				return perr
			}
			for _, imp := range f.Imports {
				if imp.Path.Value == organ {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports the understanding organ — an LLM must never sit in the money path", rel)
				}
			}
			checked++
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pkg, err)
		}
	}
	// The guard must actually have read files; a silently-empty walk would make this
	// test vacuous (always green while proving nothing).
	if checked == 0 {
		t.Fatal("guard parsed 0 files — it is not actually checking anything")
	}
}

// repoRoot locates the module root from the test's working directory so the guard
// reads the REAL source tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
