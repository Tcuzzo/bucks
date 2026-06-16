package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
)

// TestUpdateNeverBlocks_Wizard asserts the never-stall INVARIANT behaviorally:
// every keystroke through every wizard step returns promptly (well under a tight
// budget) with a model and an optional command — never a synchronous call that
// would stall the UI. We walk the whole wizard and time each Update.
func TestUpdateNeverBlocks_Wizard(t *testing.T) {
	const budget = 20 * time.Millisecond // generous; real Updates are microseconds

	// Drive a realistic sequence covering every step + an invalid input branch.
	msgs := []tea.Msg{
		runes("v"), enter(), // welcome: toggle voice, begin
		enter(),                                           // telegram: reject empty (error branch)
		runes("123456789:AAH-validlookingtoken"), enter(), // telegram ok
		runes("9"), runes("1"), enter(), // llm: ignore bad, pick, confirm
		runes("1"), runes("PKtestbrokerkey123"), enter(), // broker: pick + key -> secret sub-prompt
		runes("SKtestbrokersecret456"), enter(), // broker: secret -> intake
	}
	m := NewWizard()
	for i, msg := range msgs {
		start := time.Now()
		next, _ := m.Update(msg)
		elapsed := time.Since(start)
		if elapsed > budget {
			t.Fatalf("wizard Update[%d] took %s (> %s budget) — a blocking call slipped into Update", i, elapsed, budget)
		}
		m = next.(WizardModel)
	}
	// Finish the intake + safety, still timing.
	for _, ans := range []string{"moderate", "25000", "swing", "tech", "0.20", "goal", "0.01", "0.03", "2", "8"} {
		for _, msg := range []tea.Msg{runes(ans), enter()} {
			start := time.Now()
			next, _ := m.Update(msg)
			if d := time.Since(start); d > budget {
				t.Fatalf("intake Update took %s (> %s)", d, budget)
			}
			m = next.(WizardModel)
		}
	}
	start := time.Now()
	if _, _ = m.Update(enter()); time.Since(start) > budget { // safety finish builds the playbook
		t.Fatal("safety finish (playbook build) exceeded the latency budget")
	}
}

// TestUpdateNeverBlocks_Dashboard asserts the dashboard Update (the snapshot swap)
// is prompt. Applying a snapshot is a value copy, never a fetch.
func TestUpdateNeverBlocks_Dashboard(t *testing.T) {
	const budget = 20 * time.Millisecond
	now := time.Now()
	snap := Snapshot{
		Now:    now,
		Report: channel.Report{Equity: dec("1"), RealizedPL: dec("0"), UnrealizedPL: dec("0")},
		Health: Health{LastHeartbeat: now},
	}
	m := NewDashboard()
	start := time.Now()
	if _, _ = m.Update(SnapshotMsg{Snapshot: snap}); time.Since(start) > budget {
		t.Fatal("dashboard snapshot Update exceeded the latency budget")
	}
}

// TestNoBlockingIOInUpdatePaths is the STRUCTURAL proof of the never-stall
// invariant. The real logic of Update lives in its helpers (updateWelcome,
// updateTelegram, updateBroker, updateBrokerSecret, updateIntake, updateSafety,
// editInput, advance, back, the View renderers, …), so checking only Update's own
// body would miss a blocking call moved one hop away. Because this whole package is
// a pure-render bubbletea model — no function here may touch the network, disk, or
// clock — the robust check is: scan EVERY function/method body in every non-test
// file for a banned blocking selector, and ban the blocking import packages
// (including "os") at the import level as belt-and-suspenders.
//
// This is a real assertion, not a grep in a comment: it walks the AST.
func TestNoBlockingIOInUpdatePaths(t *testing.T) {
	bannedSelectors := map[string]bool{
		"http.Get": true, "http.Post": true, "http.NewRequest": true,
		"net.Dial": true, "time.Sleep": true,
		"os.ReadFile": true, "os.Open": true, "os.WriteFile": true, "os.Create": true,
		"sql.Open": true,
	}
	bannedImports := map[string]bool{
		`"net"`: true, `"net/http"`: true, `"database/sql"`: true, `"os"`: true,
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checkedFns := 0
	sawUpdate := false
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}

		// 1) No blocking I/O import in any model source file at all (the package's
		// logic is pure; only cmd/ wires the terminal/disk). "os" is banned here too
		// so any os.* usage is caught at the import level, not just by selector.
		for _, imp := range f.Imports {
			if bannedImports[imp.Path.Value] {
				t.Errorf("%s imports blocking package %s — the tui package must stay I/O-free (model the slow path as a tea.Cmd)", name, imp.Path.Value)
			}
		}

		// 2) Walk EVERY function/method body in the file (Update AND every helper it
		// reaches) for banned blocking selectors. None of them may do blocking I/O.
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			checkedFns++
			if fn.Name.Name == "Update" {
				sawUpdate = true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						qualified := pkg.Name + "." + sel.Sel.Name
						if bannedSelectors[qualified] {
							t.Errorf("%s: %s calls blocking %s — must be modeled as a tea.Cmd instead", name, fn.Name.Name, qualified)
						}
					}
				}
				return true
			})
			return true
		})
	}
	if !sawUpdate {
		t.Fatal("no Update method found to check — the structural proof scanned nothing")
	}
	if checkedFns == 0 {
		t.Fatal("scanned zero function bodies — the structural proof is vacuous")
	}
}
