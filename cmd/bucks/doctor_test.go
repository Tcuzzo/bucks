package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/updater"
)

// ---------------------------------------------------------------------------
// parseOutdatedModules
// ---------------------------------------------------------------------------

// TestParseOutdatedModules_Empty proves an empty stream returns no results.
func TestParseOutdatedModules_Empty(t *testing.T) {
	got := parseOutdatedModules([]byte(""))
	if len(got) != 0 {
		t.Fatalf("empty input: want 0, got %d", len(got))
	}
}

// TestParseOutdatedModules_NoUpdate proves modules without an Update field are
// filtered out — they are current and we do not warn about them.
func TestParseOutdatedModules_NoUpdate(t *testing.T) {
	input := `{
  "Path": "github.com/foo/bar",
  "Version": "v1.2.3"
}`
	got := parseOutdatedModules([]byte(input))
	if len(got) != 0 {
		t.Fatalf("no-update module must be filtered, got %v", got)
	}
}

// TestParseOutdatedModules_OneOutdated proves a module WITH an Update block is
// returned with the correct Path, Version, and Update.Version.
func TestParseOutdatedModules_OneOutdated(t *testing.T) {
	input := `{
  "Path": "github.com/foo/bar",
  "Version": "v1.2.3",
  "Update": {
    "Path": "github.com/foo/bar",
    "Version": "v1.3.0"
  }
}`
	got := parseOutdatedModules([]byte(input))
	if len(got) != 1 {
		t.Fatalf("want 1 outdated module, got %d: %v", len(got), got)
	}
	if got[0].Path != "github.com/foo/bar" {
		t.Errorf("Path = %q, want github.com/foo/bar", got[0].Path)
	}
	if got[0].Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got[0].Version)
	}
	if got[0].Update != "v1.3.0" {
		t.Errorf("Update = %q, want v1.3.0", got[0].Update)
	}
}

// TestParseOutdatedModules_MixedStream proves a multi-object JSON stream
// (concatenated, as go list -m -u -json all produces) is parsed correctly:
// only modules with a non-empty Update are returned.
func TestParseOutdatedModules_MixedStream(t *testing.T) {
	input := `{
  "Path": "github.com/alpha/one",
  "Version": "v1.0.0",
  "Update": {
    "Path": "github.com/alpha/one",
    "Version": "v1.1.0"
  }
}
{
  "Path": "github.com/beta/two",
  "Version": "v2.0.0"
}
{
  "Path": "github.com/gamma/three",
  "Version": "v3.0.0",
  "Update": {
    "Path": "github.com/gamma/three",
    "Version": "v3.5.0"
  }
}`
	got := parseOutdatedModules([]byte(input))
	if len(got) != 2 {
		t.Fatalf("want 2 outdated modules, got %d: %v", len(got), got)
	}
	paths := map[string]bool{got[0].Path: true, got[1].Path: true}
	if !paths["github.com/alpha/one"] {
		t.Error("missing github.com/alpha/one")
	}
	if !paths["github.com/gamma/three"] {
		t.Error("missing github.com/gamma/three")
	}
	if paths["github.com/beta/two"] {
		t.Error("current module github.com/beta/two must not appear")
	}
}

// ---------------------------------------------------------------------------
// parseGovulncheckVulns
// ---------------------------------------------------------------------------

// TestParseGovulncheckVulns_Empty proves an empty stream returns no IDs.
func TestParseGovulncheckVulns_Empty(t *testing.T) {
	got := parseGovulncheckVulns([]byte(""))
	if len(got) != 0 {
		t.Fatalf("empty input: want 0, got %d", len(got))
	}
}

// TestParseGovulncheckVulns_NoFindings proves non-finding messages (config,
// progress) are ignored.
func TestParseGovulncheckVulns_NoFindings(t *testing.T) {
	input := `{"message": {"config": {"go_version": "go1.24"}}}
{"message": {"progress": {"message": "Scanning..."}}}
`
	got := parseGovulncheckVulns([]byte(input))
	if len(got) != 0 {
		t.Fatalf("no findings: want 0, got %v", got)
	}
}

// TestParseGovulncheckVulns_Deduplication proves the same OSV ID appearing in
// multiple finding messages is returned only once.
func TestParseGovulncheckVulns_Deduplication(t *testing.T) {
	input := `{"message": {"finding": {"osv": "GO-2024-1234", "trace": []}}}
{"message": {"finding": {"osv": "GO-2024-1234", "trace": []}}}
{"message": {"finding": {"osv": "GO-2024-5678", "trace": []}}}
`
	got := parseGovulncheckVulns([]byte(input))
	if len(got) != 2 {
		t.Fatalf("want 2 distinct vulns, got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["GO-2024-1234"] {
		t.Error("missing GO-2024-1234")
	}
	if !seen["GO-2024-5678"] {
		t.Error("missing GO-2024-5678")
	}
}

// TestParseGovulncheckVulns_MixedMessages proves finding messages are extracted
// even when mixed with config/progress messages.
func TestParseGovulncheckVulns_MixedMessages(t *testing.T) {
	input := `{"message": {"config": {"go_version": "go1.24"}}}
{"message": {"finding": {"osv": "GO-2024-9999", "trace": []}}}
{"message": {"progress": {"message": "done"}}}
`
	got := parseGovulncheckVulns([]byte(input))
	if len(got) != 1 || got[0] != "GO-2024-9999" {
		t.Fatalf("want [GO-2024-9999], got %v", got)
	}
}

func TestRunGovulncheckRejectsScannerFailureAndIncompleteJSON(t *testing.T) {
	tests := []struct {
		name   string
		output string
		exit   int
	}{
		{name: "non findings exit code", output: `{"config":{"protocol_version":"v1.0.0"}}`, exit: 2},
		{name: "truncated successful stream", output: `{"config":{"protocol_version":"v1.0.0"}`, exit: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			fake := filepath.Join(binDir, "govulncheck")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %q\nexit %d\n", tt.output, tt.exit)
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake govulncheck: %v", err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			if _, err := runGovulncheck(); err == nil {
				t.Fatal("failed or incomplete vulnerability scan must return an error, never a clean result")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// versionOutdated
// ---------------------------------------------------------------------------

// TestVersionOutdated_Same proves identical versions return false.
func TestVersionOutdated_Same(t *testing.T) {
	if versionOutdated("v1.2.3", "v1.2.3") {
		t.Error("same version should not be outdated")
	}
}

// TestVersionOutdated_Older proves an older current version returns true.
func TestVersionOutdated_Older(t *testing.T) {
	cases := []struct{ cur, latest string }{
		{"v1.0.0", "v1.1.0"},
		{"v1.2.3", "v2.0.0"},
		{"1.0.0", "1.0.1"}, // no leading v
	}
	for _, c := range cases {
		if !versionOutdated(c.cur, c.latest) {
			t.Errorf("versionOutdated(%q, %q) = false, want true", c.cur, c.latest)
		}
	}
}

// TestVersionOutdated_Newer proves a current version AHEAD of latest returns false.
func TestVersionOutdated_Newer(t *testing.T) {
	if versionOutdated("v2.0.0", "v1.9.9") {
		t.Error("ahead-of-latest must not be outdated")
	}
}

// TestVersionOutdated_DevIsAlwaysOutdated proves dev/unversioned current builds
// are always considered outdated.
func TestVersionOutdated_DevIsAlwaysOutdated(t *testing.T) {
	for _, cur := range []string{"dev", "Dev", "DEV", ""} {
		if !versionOutdated(cur, "v1.0.0") {
			t.Errorf("versionOutdated(%q, v1.0.0) = false, want true (dev always outdated)", cur)
		}
	}
}

// ---------------------------------------------------------------------------
// summarize
// ---------------------------------------------------------------------------

// TestSummarize_NoIssues proves the summary correctly reports zero issues.
func TestSummarize_NoIssues(t *testing.T) {
	s := summarize(nil, nil)
	if !strings.Contains(s, "0 issue") {
		t.Errorf("zero-issues summary = %q, want '0 issue(s) found'", s)
	}
}

// TestSummarize_WithModulesAndVulns proves the summary counts both outdated
// modules and vulnerabilities and reports the combined count as issues.
func TestSummarize_WithModulesAndVulns(t *testing.T) {
	mods := []ModuleStatus{
		{Path: "github.com/foo/bar", Version: "v1.0.0", Update: "v1.1.0"},
		{Path: "github.com/baz/qux", Version: "v2.0.0", Update: "v2.1.0"},
	}
	vulns := []string{"GO-2024-1234"}
	s := summarize(mods, vulns)
	// Must mention both counts.
	if !strings.Contains(s, "2") {
		t.Errorf("summary missing outdated module count; got %q", s)
	}
	if !strings.Contains(s, "1") {
		t.Errorf("summary missing vuln count; got %q", s)
	}
}

// TestSummarize_VulnsOnly proves a summary with only vulns still reports issues.
func TestSummarize_VulnsOnly(t *testing.T) {
	s := summarize(nil, []string{"GO-2024-0001"})
	if strings.Contains(s, "0 issue") {
		t.Errorf("vuln-only summary claims 0 issues; got %q", s)
	}
}

// ---------------------------------------------------------------------------
// doctorFixCommand
// ---------------------------------------------------------------------------

// TestDoctorFixCommand_FromSource proves fromSource=true returns a go get command.
func TestDoctorFixCommand_FromSource(t *testing.T) {
	cmd := doctorFixCommand(true)
	if len(cmd) == 0 {
		t.Fatal("fix command must not be empty")
	}
	if cmd[0] != "go" {
		t.Errorf("fromSource fix[0] = %q, want go", cmd[0])
	}
	found := false
	for _, arg := range cmd {
		if arg == "-u" || arg == "./..." {
			found = true
		}
	}
	if !found {
		t.Errorf("fromSource fix command should include -u or ./...; got %v", cmd)
	}
}

// TestDoctorFixCommand_Binary proves fromSource=false returns a bucks update command.
func TestDoctorFixCommand_Binary(t *testing.T) {
	cmd := doctorFixCommand(false)
	if len(cmd) == 0 {
		t.Fatal("fix command must not be empty")
	}
	if cmd[0] != "bucks" {
		t.Errorf("binary fix[0] = %q, want bucks", cmd[0])
	}
	if len(cmd) < 2 || cmd[1] != "update" {
		t.Errorf("binary fix command should be [bucks update]; got %v", cmd)
	}
}

func TestRunDoctorCore_ReturnsErrorForVulnerabilitiesWithoutProcessExit(t *testing.T) {
	if os.Getenv("BUCKS_DOCTOR_VULN_HELPER") == "1" {
		runDoctorVulnerabilityHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunDoctorCore_ReturnsErrorForVulnerabilitiesWithoutProcessExit")
	cmd.Env = append(os.Environ(), "BUCKS_DOCTOR_VULN_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor helper exited instead of returning an error: %v\n%s", err, out)
	}
}

func runDoctorVulnerabilityHelper(t *testing.T) {
	t.Helper()

	tmp := t.TempDir()
	goMod := []byte("module example.com/bucks-doctor-test\n\ngo 1.24\n")
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), goMod, 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp module: %v", err)
	}

	binDir := t.TempDir()
	fakeGo := filepath.Join(binDir, "go")
	goScript := `#!/bin/sh
case "$1" in
  version)
    printf '%s\n' 'go version go1.24.0 linux/amd64'
    ;;
  list)
    printf '%s\n' '{"Path":"example.com/bucks-doctor-test","Version":"v0.0.0"}'
    ;;
  *)
    printf 'unexpected fake go command: %s\n' "$*" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(fakeGo, []byte(goScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	fakeGovulncheck := filepath.Join(binDir, "govulncheck")
	script := `#!/bin/sh
printf '%s\n' '{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck"}}'
printf '%s\n' '{"message":{"finding":{"osv":"GO-2026-9999"}}}'
exit 3
`
	if err := os.WriteFile(fakeGovulncheck, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake govulncheck: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
	}))
	defer srv.Close()
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
	)

	var out bytes.Buffer
	err := runDoctorCore(t.Context(), u, &out, false, false)
	if err == nil {
		t.Fatalf("doctor must return an error when vulnerabilities are found; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "vulnerabilities") {
		t.Fatalf("doctor error should mention vulnerabilities, got: %v", err)
	}
	if !strings.Contains(out.String(), "GO-2026-9999") {
		t.Fatalf("doctor output should include the vulnerability id, got:\n%s", out.String())
	}
}

func TestRunDoctorCoreRunsSourceScansOnceWithoutFix(t *testing.T) {
	if os.Getenv("BUCKS_DOCTOR_SCAN_COUNT_HELPER") == "1" {
		runDoctorScanCountHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunDoctorCoreRunsSourceScansOnceWithoutFix")
	cmd.Env = append(os.Environ(), "BUCKS_DOCTOR_SCAN_COUNT_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor scan-count helper failed: %v\n%s", err, out)
	}
}

func TestRunDoctorCoreDoesNotRetryFailedSourceScansWithoutFix(t *testing.T) {
	if os.Getenv("BUCKS_DOCTOR_FAILED_SCAN_COUNT_HELPER") == "1" {
		runDoctorScanCountHelperWithFailures(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunDoctorCoreDoesNotRetryFailedSourceScansWithoutFix")
	cmd.Env = append(os.Environ(), "BUCKS_DOCTOR_FAILED_SCAN_COUNT_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed-scan helper failed: %v\n%s", err, out)
	}
}

func runDoctorScanCountHelper(t *testing.T) {
	t.Helper()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/bucks-doctor-test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp module: %v", err)
	}

	binDir := t.TempDir()
	goCount := filepath.Join(tmp, "go-list-count")
	vulnCount := filepath.Join(tmp, "govulncheck-count")
	fakeGo := filepath.Join(binDir, "go")
	goScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  version)
    printf '%%s\n' 'go version go1.24.0 linux/amd64'
    ;;
  list)
    printf 'x\n' >> %q
    printf '%%s\n' '{"Path":"example.com/bucks-doctor-test","Version":"v0.0.0"}'
    ;;
  *)
    printf 'unexpected fake go command: %%s\n' "$*" >&2
    exit 2
    ;;
esac
`, goCount)
	if err := os.WriteFile(fakeGo, []byte(goScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	fakeGovulncheck := filepath.Join(binDir, "govulncheck")
	vulnScript := fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
printf '%%s\n' '{"config":{"protocol_version":"v1.0.0","go_version":"go1.24.0"}}'
`, vulnCount)
	if err := os.WriteFile(fakeGovulncheck, []byte(vulnScript), 0o755); err != nil {
		t.Fatalf("write fake govulncheck: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
	}))
	defer srv.Close()
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
	)

	var out bytes.Buffer
	if err := runDoctorCore(t.Context(), u, &out, false, false); err != nil {
		t.Fatalf("runDoctorCore: %v\n%s", err, out.String())
	}

	assertLineCount(t, goCount, 1)
	assertLineCount(t, vulnCount, 1)
}

func runDoctorScanCountHelperWithFailures(t *testing.T) {
	t.Helper()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/bucks-doctor-test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp module: %v", err)
	}

	binDir := t.TempDir()
	goCount := filepath.Join(tmp, "go-list-count")
	vulnCount := filepath.Join(tmp, "govulncheck-count")
	fakeGo := filepath.Join(binDir, "go")
	goScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  version)
    printf '%%s\n' 'go version go1.24.0 linux/amd64'
    ;;
  list)
    printf 'x\n' >> %q
    printf 'go list failed\n' >&2
    exit 2
    ;;
  *)
    printf 'unexpected fake go command: %%s\n' "$*" >&2
    exit 2
    ;;
esac
`, goCount)
	if err := os.WriteFile(fakeGo, []byte(goScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	fakeGovulncheck := filepath.Join(binDir, "govulncheck")
	vulnScript := fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
printf 'govulncheck failed\n' >&2
exit 2
`, vulnCount)
	if err := os.WriteFile(fakeGovulncheck, []byte(vulnScript), 0o755); err != nil {
		t.Fatalf("write fake govulncheck: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
	}))
	defer srv.Close()
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
	)

	var out bytes.Buffer
	if err := runDoctorCore(t.Context(), u, &out, false, false); err == nil {
		t.Fatalf("runDoctorCore must return an error when vulnerability status is unknown:\n%s", out.String())
	}
	if !strings.Contains(strings.ToLower(out.String()), "unknown / not scanned") {
		t.Fatalf("failed vulnerability scan must be reported as unknown / not scanned:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Vulnerabilities (0): none") {
		t.Fatalf("failed vulnerability scan falsely reported clean:\n%s", out.String())
	}

	assertLineCount(t, goCount, 1)
	assertLineCount(t, vulnCount, 1)
}

func assertLineCount(t *testing.T, path string, want int) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counter %s: %v", path, err)
	}
	got := strings.Count(string(data), "\n")
	if got != want {
		t.Fatalf("%s count = %d, want %d", filepath.Base(path), got, want)
	}
}
