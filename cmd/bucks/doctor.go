package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"bucks/internal/updater"
)

// ModuleStatus holds one module's current and available-update versions.
type ModuleStatus struct {
	Path    string
	Version string
	Update  string // latest available version; empty means up to date
}

// runDoctorStdio is the production `bucks doctor` entry point.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "apply available fixes (go get -u ./... + go mod tidy for source; bucks update for binary)")
	check := fs.Bool("check", false, "print what would be checked and the fix command without running any scans")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDoctorCore(context.Background(), updater.New(), os.Stdout, *fix, *check)
}

// runDoctorCore is the testable doctor flow with all I/O injected.
func runDoctorCore(ctx context.Context, u *updater.Updater, out io.Writer, fix, checkOnly bool) error {
	if checkOnly {
		return runDoctorCheck(out)
	}

	fmt.Fprintln(out, "bucks doctor")
	fmt.Fprintln(out, "============")

	// ---- BINARY/RUNTIME section ----
	fmt.Fprintln(out, "\n[BINARY/RUNTIME]")

	// BUCKS version vs latest GitHub release.
	current := u.CurrentVersion()
	rel, relErr := u.CheckLatest(ctx)
	if relErr != nil {
		fmt.Fprintf(out, "  bucks version:  %s (could not check latest: %v)\n", current, relErr)
	} else if rel.IsNewer {
		fmt.Fprintf(out, "  bucks version:  %s → %s (update available — run `bucks update`)\n",
			describeCurrent(current, rel.IsDevCur), rel.Tag)
	} else if rel.IsDevCur {
		fmt.Fprintf(out, "  bucks version:  dev (un-versioned build) — latest is %s\n", rel.Tag)
	} else {
		fmt.Fprintf(out, "  bucks version:  %s (up to date)\n", current)
	}

	// Go toolchain version, informational.
	if goExe, err := exec.LookPath("go"); err == nil {
		goVer, err := goToolchainVersion(goExe)
		if err != nil {
			fmt.Fprintf(out, "  Go toolchain:   (could not run: %v)\n", err)
		} else {
			fmt.Fprintf(out, "  Go toolchain:   %s\n", goVer)
		}
	} else {
		fmt.Fprintln(out, "  Go toolchain:   not found on PATH")
	}

	// ---- SOURCE/DEPS section ----
	hasGoMod := fileExists("go.mod")
	hasGo := hasGoExe()

	fmt.Fprintln(out)
	if !hasGoMod {
		fmt.Fprintln(out, "[SOURCE/DEPS]  (no go.mod in current directory — skipping)")
	} else if !hasGo {
		fmt.Fprintln(out, "[SOURCE/DEPS]  (go not found on PATH — skipping)")
	} else {
		fmt.Fprintln(out, "[SOURCE/DEPS]  (go.mod found)")

		// Outdated modules.
		fmt.Fprint(out, "  Checking modules... ")
		modOut, modErr := runGoListModules()
		if modErr != nil {
			fmt.Fprintf(out, "error: %v\n", modErr)
		} else {
			mods := parseOutdatedModules(modOut)
			if len(mods) == 0 {
				fmt.Fprintln(out, "\n  Outdated modules (0): all up to date")
			} else {
				fmt.Fprintf(out, "\n  Outdated modules (%d):\n", len(mods))
				for _, m := range mods {
					fmt.Fprintf(out, "    %-50s  %s → %s\n", m.Path, m.Version, m.Update)
				}
			}
		}

		// Vulnerabilities.
		fmt.Fprint(out, "  Checking vulnerabilities... ")
		vulnOut, vulnErr := runGovulncheck()
		if vulnErr != nil {
			fmt.Fprintf(out, "error: %v\n", vulnErr)
		} else {
			vulns := parseGovulncheckVulns(vulnOut)
			if len(vulns) == 0 {
				fmt.Fprintln(out, "\n  Vulnerabilities (0): none")
			} else {
				fmt.Fprintf(out, "\n  Vulnerabilities (%d):\n", len(vulns))
				for _, v := range vulns {
					fmt.Fprintf(out, "    %s\n", v)
				}
			}
		}

		// Apply fix if requested.
		if fix {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  Applying fix (go get -u ./... && go mod tidy)...")
			if err := runGoGetUpdate(); err != nil {
				fmt.Fprintf(out, "  Fix failed: %v\n", err)
			} else {
				fmt.Fprintln(out, "  Fix applied. Re-run `bucks doctor` to verify.")
			}
		}
	}

	// ---- Summary ----
	fmt.Fprintln(out)

	// Gather final counts for summary (re-parse if we ran the scans).
	var finalMods []ModuleStatus
	var finalVulns []string
	if hasGoMod && hasGo {
		if modOut, err := runGoListModules(); err == nil {
			finalMods = parseOutdatedModules(modOut)
		}
		if vulnOut, err := runGovulncheck(); err == nil {
			finalVulns = parseGovulncheckVulns(vulnOut)
		}
	}
	// Binary outdated also counts.
	if relErr == nil && rel.IsNewer {
		finalMods = append(finalMods, ModuleStatus{Path: "bucks (binary)", Update: rel.Tag})
	}

	fmt.Fprintln(out, summarize(finalMods, finalVulns))

	if len(finalVulns) > 0 {
		os.Exit(1)
	}
	return nil
}

// runDoctorCheck prints what doctor would check plus the fix command, without
// running any scans.
func runDoctorCheck(out io.Writer) error {
	hasGoMod := fileExists("go.mod")
	hasGo := hasGoExe()
	fromSource := hasGoMod && hasGo

	fmt.Fprintln(out, "bucks doctor --check")
	fmt.Fprintln(out, "====================")
	fmt.Fprintln(out, "Would check:")
	fmt.Fprintln(out, "  [BINARY/RUNTIME]")
	fmt.Fprintln(out, "    - bucks version vs latest GitHub release")
	fmt.Fprintln(out, "    - Go toolchain version (informational)")
	if fromSource {
		fmt.Fprintln(out, "  [SOURCE/DEPS]  (go.mod present)")
		fmt.Fprintln(out, "    - Outdated modules:    go list -m -u -json all")
		fmt.Fprintln(out, "    - Vulnerabilities:     govulncheck -json ./...")
	} else {
		fmt.Fprintln(out, "  [SOURCE/DEPS]  (skipped — no go.mod or go not in PATH)")
	}
	fix := doctorFixCommand(fromSource)
	fmt.Fprintf(out, "Fix command: %s\n", strings.Join(fix, " "))
	return nil
}

// ---------------------------------------------------------------------------
// Pure functions (testable without side effects)
// ---------------------------------------------------------------------------

// parseOutdatedModules parses the concatenated-JSON-object stream produced by
//
//	go list -m -u -json all
//
// and returns only modules that have a non-empty Update.Version.
func parseOutdatedModules(jsonStream []byte) []ModuleStatus {
	// go list -m -u -json all emits a stream of JSON objects, NOT a JSON array.
	// We decode them one by one using a streaming decoder.
	dec := json.NewDecoder(bytes.NewReader(jsonStream))

	type updateBlock struct {
		Path    string `json:"Path"`
		Version string `json:"Version"`
	}
	type goMod struct {
		Path    string       `json:"Path"`
		Version string       `json:"Version"`
		Update  *updateBlock `json:"Update"`
	}

	var result []ModuleStatus
	for dec.More() {
		var m goMod
		if err := dec.Decode(&m); err != nil {
			continue // skip malformed objects
		}
		if m.Update != nil && m.Update.Version != "" {
			result = append(result, ModuleStatus{
				Path:    m.Path,
				Version: m.Version,
				Update:  m.Update.Version,
			})
		}
	}
	return result
}

// parseGovulncheckVulns parses the JSON stream produced by govulncheck -json
// and returns a deduplicated slice of OSV IDs (like "GO-2024-xxxx").
func parseGovulncheckVulns(jsonStream []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(jsonStream))

	// govulncheck -json emits a stream of {"message": {<type>: {...}}} objects.
	type finding struct {
		OSV string `json:"osv"`
	}
	type message struct {
		Finding *finding `json:"finding"`
	}
	type entry struct {
		Message message `json:"message"`
	}

	seen := map[string]bool{}
	var result []string
	for dec.More() {
		var e entry
		if err := dec.Decode(&e); err != nil {
			continue
		}
		if e.Message.Finding != nil && e.Message.Finding.OSV != "" {
			id := e.Message.Finding.OSV
			if !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	return result
}

// versionOutdated reports whether current is older than latest.
//   - "dev" / "" current is always considered outdated (un-versioned build).
//   - Identical versions return false.
//   - If either version cannot be parsed as semver, returns false (no false alarms).
func versionOutdated(current, latest string) bool {
	cur := strings.ToLower(strings.TrimSpace(current))
	if cur == "" || cur == "dev" {
		return true
	}
	// Strip leading 'v' for both and compare numerically via the updater's own
	// parseSemver, which is in the same package (package main).
	// We access it indirectly by using the newer() helper that updater exposes as
	// an internal — but since we're in a different package we use our own mini
	// comparison.
	return semverLess(current, latest)
}

// semverLess returns true if a < b (semver). Strips a leading 'v'/'V'.
// Returns false if either cannot be parsed.
func semverLess(a, b string) bool {
	av, aok := parseMiniSemver(a)
	bv, bok := parseMiniSemver(b)
	if !aok || !bok {
		return false
	}
	switch {
	case av[0] != bv[0]:
		return av[0] < bv[0]
	case av[1] != bv[1]:
		return av[1] < bv[1]
	default:
		return av[2] < bv[2]
	}
}

// parseMiniSemver parses a semver tag into [major, minor, patch]. Leading 'v'
// stripped; pre-release/build metadata stripped. Returns ok=false on parse error.
func parseMiniSemver(tag string) ([3]int, bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	var out [3]int
	for i, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return [3]int{}, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// summarize returns a one-line summary string with counts for outdated modules
// and vulnerabilities.
func summarize(modules []ModuleStatus, vulns []string) string {
	total := len(modules) + len(vulns)
	return fmt.Sprintf("Summary: %d issue(s) found. (outdated modules: %d, vulnerabilities: %d)",
		total, len(modules), len(vulns))
}

// doctorFixCommand returns the command slice to fix the issues found.
// fromSource=true means a go.mod is present; fromSource=false means binary-only.
func doctorFixCommand(fromSource bool) []string {
	if fromSource {
		return []string{"go", "get", "-u", "./..."}
	}
	return []string{"bucks", "update"}
}

// ---------------------------------------------------------------------------
// Helpers (exec wrappers)
// ---------------------------------------------------------------------------

// describeCurrent renders the current version for user-facing messages.
func describeCurrent(v string, isDev bool) string {
	if isDev {
		return "dev"
	}
	return v
}

// goToolchainVersion runs `go version` and returns the short form like "go1.24.2".
func goToolchainVersion(goExe string) (string, error) {
	out, err := exec.Command(goExe, "version").Output()
	if err != nil {
		return "", err
	}
	// Output is "go version go1.24.2 linux/amd64"
	fields := strings.Fields(string(out))
	if len(fields) >= 3 {
		return fields[2], nil
	}
	return strings.TrimSpace(string(out)), nil
}

// runGoListModules runs `go list -m -u -json all` and returns stdout.
func runGoListModules() ([]byte, error) {
	return exec.Command("go", "list", "-m", "-u", "-json", "all").Output()
}

// runGoGetUpdate runs `go get -u ./...` followed by `go mod tidy`.
func runGoGetUpdate() error {
	get := exec.Command("go", "get", "-u", "./...")
	get.Stdout = os.Stderr
	get.Stderr = os.Stderr
	if err := get.Run(); err != nil {
		return fmt.Errorf("go get -u ./...: %w", err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Stdout = os.Stderr
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	return nil
}

// runGovulncheck runs govulncheck -json ./...; if govulncheck is not in PATH it
// installs it first, then re-runs.
// govulncheckExe returns the path to govulncheck: PATH first, then GOPATH/bin,
// then GOBIN. Returns "" if none found.
func govulncheckExe() string {
	if p, err := exec.LookPath("govulncheck"); err == nil {
		return p
	}
	// Try $GOPATH/bin and $GOBIN (common when GOPATH/bin is not on PATH).
	candidates := []string{}
	if gobin, err := exec.Command("go", "env", "GOBIN").Output(); err == nil {
		if p := strings.TrimSpace(string(gobin)); p != "" {
			candidates = append(candidates, p+"/govulncheck")
		}
	}
	if gopath, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if p := strings.TrimSpace(string(gopath)); p != "" {
			candidates = append(candidates, p+"/bin/govulncheck")
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

func runGovulncheck() ([]byte, error) {
	exe := govulncheckExe()
	if exe == "" {
		// Auto-install then re-locate.
		fmt.Fprint(os.Stderr, "\n  (govulncheck not found — installing golang.org/x/vuln/cmd/govulncheck@latest...)\n")
		install := exec.Command("go", "install", "golang.org/x/vuln/cmd/govulncheck@latest")
		install.Stdout = os.Stderr
		install.Stderr = os.Stderr
		if err2 := install.Run(); err2 != nil {
			return nil, fmt.Errorf("govulncheck not in PATH and install failed: %w", err2)
		}
		exe = govulncheckExe()
		if exe == "" {
			return nil, fmt.Errorf("govulncheck installed but still not found — add $(go env GOPATH)/bin to PATH")
		}
	}
	// govulncheck exit code 3 means findings exist; that's not an exec error.
	cmd := exec.Command(exe, "-json", "./...")
	out, err := cmd.Output()
	if err != nil {
		// exitError with findings is OK — we parse the JSON ourselves.
		if _, ok := err.(*exec.ExitError); ok {
			return out, nil
		}
		return nil, err
	}
	return out, nil
}

// fileExists reports whether path exists as a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// hasGoExe reports whether `go` is on PATH.
func hasGoExe() bool {
	_, err := exec.LookPath("go")
	return err == nil
}
