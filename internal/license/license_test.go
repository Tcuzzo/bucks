package license

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassify proves the SPDX classifier: permissive ids ALLOW, copyleft ids DENY,
// and anything not curated is UNKNOWN (which fails the gate). Case/space-insensitive.
func TestClassify(t *testing.T) {
	allowed := []string{"MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC", "Apache-2.0", " mit ", "apache-2.0"}
	for _, id := range allowed {
		if got := Classify(id); got != ClassAllowed {
			t.Errorf("Classify(%q) = %v, want allowed", id, got)
		}
	}
	copyleft := []string{"AGPL-3.0", "AGPL-3.0-only", "GPL-2.0", "GPL-3.0-or-later", "LGPL-2.1", "MPL-2.0", "agpl-3.0"}
	for _, id := range copyleft {
		if got := Classify(id); got != ClassCopyleft {
			t.Errorf("Classify(%q) = %v, want copyleft", id, got)
		}
	}
	unknown := []string{"", "WTFPL", "Proprietary", "SomeNewLicense-9.9"}
	for _, id := range unknown {
		if got := Classify(id); got != ClassUnknown {
			t.Errorf("Classify(%q) = %v, want unknown", id, got)
		}
	}
}

// TestScanSyntheticAGPLFails is the core gate proof: a SYNTHETIC module carrying an
// AGPL license makes Scan FAIL, and the failure NAMES the offending module. This is
// what stops an AGPL dependency from ever shipping.
func TestScanSyntheticAGPLFails(t *testing.T) {
	mods := map[string]string{
		"github.com/example/clean-lib": "MIT",
		"github.com/evil/agpl-lib":     "AGPL-3.0", // the poison pill
	}
	rep, err := Scan(mods)
	if err == nil {
		t.Fatal("Scan must FAIL when an AGPL module is present, got nil error")
	}
	if rep.Passed() {
		t.Fatal("Report.Passed() must be false with an AGPL module")
	}
	if len(rep.Denied) != 1 {
		t.Fatalf("want exactly 1 denied finding, got %d", len(rep.Denied))
	}
	if rep.Denied[0].Module != "github.com/evil/agpl-lib" {
		t.Errorf("denied module = %q, want the AGPL one", rep.Denied[0].Module)
	}
	if !strings.Contains(err.Error(), "github.com/evil/agpl-lib") {
		t.Errorf("error must name the offending module, got: %v", err)
	}
	if !strings.Contains(err.Error(), "AGPL-3.0") {
		t.Errorf("error must name the offending license, got: %v", err)
	}
}

// TestScanUnknownLicenseFails proves the safe default: a module with no curated /
// reviewed license is UNKNOWN and FAILS — you cannot ship an unreviewed license.
func TestScanUnknownLicenseFails(t *testing.T) {
	mods := map[string]string{
		"github.com/ok/lib":      "MIT",
		"github.com/mystery/lib": "", // never reviewed -> unknown -> fail
	}
	rep, err := Scan(mods)
	if err == nil {
		t.Fatal("Scan must FAIL on an unknown (unreviewed) license")
	}
	if len(rep.Unknown) != 1 || rep.Unknown[0].Module != "github.com/mystery/lib" {
		t.Fatalf("want the mystery lib flagged unknown, got %+v", rep.Unknown)
	}
	if !strings.Contains(err.Error(), "github.com/mystery/lib") {
		t.Errorf("error must name the unknown module, got: %v", err)
	}
}

// TestScanCuratedRealTreePasses is the REAL-tree proof: BUCKS's own curated map of
// every dependency it links passes the gate — zero copyleft, zero unknown. This is
// the CI gate's green path.
func TestScanCuratedRealTreePasses(t *testing.T) {
	rep, err := ScanCurated()
	if err != nil {
		t.Fatalf("the real BUCKS dependency tree must PASS the license gate, got: %v\n%s", err, rep.Summary())
	}
	if !rep.Passed() {
		t.Fatalf("ScanCurated did not pass: %s", rep.Summary())
	}
	if len(rep.Findings) == 0 {
		t.Fatal("curated map is empty — nothing was scanned")
	}
	// Belt-and-suspenders: assert NO finding is copyleft.
	for _, f := range rep.Findings {
		if f.Class == ClassCopyleft {
			t.Errorf("copyleft slipped into the curated map: %s (%s)", f.Module, f.SPDX)
		}
	}
	t.Logf("%s", rep.Summary())
}

// TestRealGoModHasNoCopyleft asserts directly against the SHIPPED go.mod: not a
// single require line names an (A)GPL/LGPL module. This catches a copyleft dep that
// somehow got into go.mod even before the curated map is updated.
func TestRealGoModHasNoCopyleft(t *testing.T) {
	mods := requiresFromGoMod(t)
	banned := []string{"gpl", "agpl", "lgpl"}
	for _, m := range mods {
		low := strings.ToLower(m)
		for _, b := range banned {
			// crude name guard is a backstop; the curated SPDX map is authoritative.
			if strings.Contains(low, "/"+b) || strings.HasSuffix(low, "-"+b) {
				t.Errorf("go.mod names a possibly-copyleft module by name: %s", m)
			}
		}
	}
	if len(mods) == 0 {
		t.Fatal("parsed zero require lines from go.mod — parser broken")
	}
}

// TestEveryGoModRequireIsCurated forces review discipline: EVERY module declared in
// go.mod's require blocks has a curated SPDX entry in KnownLicenses. A new dependency
// with no entry fails this test, which fails the build — so an unreviewed license can
// never reach the ship. (This is the mechanism the safe-default relies on.)
func TestEveryGoModRequireIsCurated(t *testing.T) {
	mods := requiresFromGoMod(t)
	var missing []string
	for _, m := range mods {
		if _, ok := KnownLicenses[m]; !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("these go.mod require modules have NO curated license entry (read their LICENSE and add to known.go):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// requiresFromGoMod parses the module paths from the repo's go.mod require blocks.
// It walks up from the test's working dir to find go.mod (module root) so it works
// regardless of where `go test` is invoked.
func requiresFromGoMod(t *testing.T) []string {
	t.Helper()
	path := findGoMod(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	defer f.Close()

	var mods []string
	inBlock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require ") && !strings.Contains(line, "("):
			// single-line require: `require path vX.Y.Z`
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				mods = append(mods, fields[1])
			}
			continue
		}
		if !inBlock || line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 1 && strings.Contains(fields[0], ".") {
			mods = append(mods, fields[0])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan go.mod: %v", err)
	}
	return mods
}

// findGoMod locates the module's go.mod by walking up from the working directory.
func findGoMod(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		cand := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from test dir")
		}
		dir = parent
	}
}
