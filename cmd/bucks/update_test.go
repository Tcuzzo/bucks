package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"bucks/internal/updater"
)

// TestRunVersionPrints proves the real `bucks version` entry point prints the version
// line with the platform and go runtime — offline, no config.
func TestRunVersionPrints(t *testing.T) {
	var buf bytes.Buffer
	if err := runVersion(&buf); err != nil {
		t.Fatalf("runVersion: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"bucks", runtime.GOOS, runtime.GOARCH, runtime.Version()} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q: %q", want, out)
		}
	}
}

// TestDispatchVersion proves `bucks version` routes through run() without error
// (offline, makes no network call).
func TestDispatchVersion(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("run(version): %v", err)
	}
}

// fakeReplacer points the updater at a temp file for the CLI tests.
type fakeReplacer struct {
	path string
	seen bool
}

func (f *fakeReplacer) ExePath() (string, error) { return f.path, nil }
func (f *fakeReplacer) Replace(p string, b []byte) error {
	f.seen = true
	return os.WriteFile(p, b, 0o755)
}

func buildZip(t *testing.T, binName string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("BUCKS_test/" + binName)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// updateServer serves a mock latest-release with a matching zip + SHA256SUMS.
func updateServer(t *testing.T, tag string, zipBytes []byte) *httptest.Server {
	t.Helper()
	zipName := updater.AssetName(runtime.GOOS, runtime.GOARCH)
	sums := sha256Hex(zipBytes) + "  " + zipName + "\n"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/repos/Tcuzzo/bucks/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}`,
			tag, zipName, srv.URL+"/dl/zip", srv.URL+"/dl/sums")
	})
	mux.HandleFunc("/dl/zip", func(w http.ResponseWriter, r *http.Request) { w.Write(zipBytes) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	return srv
}

// TestRunUpdateCheckReportsAvailable proves `update --check` reports an available
// update WITHOUT writing anything (no Replace call).
func TestRunUpdateCheckReportsAvailable(t *testing.T) {
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	zipBytes := buildZip(t, binName, []byte("new"))
	srv := updateServer(t, "v9.9.9", zipBytes)
	defer srv.Close()

	repl := &fakeReplacer{path: filepath.Join(t.TempDir(), "bucks")}
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
		updater.WithReplacer(repl),
	)
	var out bytes.Buffer
	err := runUpdate(context.Background(), u, strings.NewReader(""), &out, updateFlags{checkOnly: true})
	if err != nil {
		t.Fatalf("runUpdate --check: %v", err)
	}
	if repl.seen {
		t.Fatal("--check must not install anything")
	}
	if !strings.Contains(out.String(), "available") {
		t.Errorf("expected availability report, got: %q", out.String())
	}
}

// TestRunUpdateCheckUpToDate proves `update --check` reports up-to-date when the
// latest is not newer, and writes nothing.
func TestRunUpdateCheckUpToDate(t *testing.T) {
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	zipBytes := buildZip(t, binName, []byte("same"))
	srv := updateServer(t, "v1.0.0", zipBytes)
	defer srv.Close()

	repl := &fakeReplacer{path: filepath.Join(t.TempDir(), "bucks")}
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
		updater.WithReplacer(repl),
	)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), u, strings.NewReader(""), &out, updateFlags{checkOnly: true}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("expected up-to-date, got: %q", out.String())
	}
	if repl.seen {
		t.Error("must not install when up to date")
	}
}

// TestRunUpdateDeclinePrompt proves a "no" answer at the y/N prompt cancels and
// installs nothing.
func TestRunUpdateDeclinePrompt(t *testing.T) {
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	zipBytes := buildZip(t, binName, []byte("new-bytes"))
	srv := updateServer(t, "v9.9.9", zipBytes)
	defer srv.Close()

	repl := &fakeReplacer{path: filepath.Join(t.TempDir(), "bucks")}
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
		updater.WithReplacer(repl),
	)
	var out bytes.Buffer
	// Answer "n".
	if err := runUpdate(context.Background(), u, strings.NewReader("n\n"), &out, updateFlags{}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if repl.seen {
		t.Fatal("declined update must not install")
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Errorf("expected cancellation, got: %q", out.String())
	}
}

// TestRunUpdateYesInstalls proves the full CLI path: confirming (via input "y")
// downloads, verifies, and installs the new binary into the injected target.
func TestRunUpdateYesInstalls(t *testing.T) {
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	newBin := []byte("installed-via-cli")
	zipBytes := buildZip(t, binName, newBin)
	srv := updateServer(t, "v9.9.9", zipBytes)
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	os.WriteFile(tmp, []byte("old"), 0o755)
	repl := &fakeReplacer{path: tmp}
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
		updater.WithReplacer(repl),
	)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), u, strings.NewReader("y\n"), &out, updateFlags{}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !repl.seen {
		t.Fatal("confirmed update should install")
	}
	got, _ := os.ReadFile(tmp)
	if !bytes.Equal(got, newBin) {
		t.Fatalf("installed bytes = %q, want %q", got, newBin)
	}
	if !strings.Contains(out.String(), "re-run") {
		t.Errorf("expected re-run hint, got: %q", out.String())
	}
}

// TestRunUpdateAbortsIfLatestChangesAfterPrompt proves the updater installs only the
// release tag the operator saw and approved. If GitHub's latest tag changes between
// the prompt and the download path, consent no longer matches the artifact.
func TestRunUpdateAbortsIfLatestChangesAfterPrompt(t *testing.T) {
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	v2Bin := []byte("approved-v2")
	v2Zip := buildZip(t, binName, v2Bin)
	v3Bin := []byte("unapproved-v3")
	v3Zip := buildZip(t, binName, v3Bin)
	zipName := updater.AssetName(runtime.GOOS, runtime.GOARCH)
	v2Sums := sha256Hex(v2Zip) + "  " + zipName + "\n"
	v3Sums := sha256Hex(v3Zip) + "  " + zipName + "\n"

	var latestHits int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/Tcuzzo/bucks/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		latestHits++
		tag := "v2.0.0"
		prefix := "v2"
		if latestHits > 1 {
			tag = "v3.0.0"
			prefix = "v3"
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}`,
			tag, zipName, srv.URL+"/dl/"+prefix+"/zip", srv.URL+"/dl/"+prefix+"/sums")
	})
	mux.HandleFunc("/dl/v2/zip", func(w http.ResponseWriter, r *http.Request) { w.Write(v2Zip) })
	mux.HandleFunc("/dl/v2/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(v2Sums)) })
	mux.HandleFunc("/dl/v3/zip", func(w http.ResponseWriter, r *http.Request) { w.Write(v3Zip) })
	mux.HandleFunc("/dl/v3/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(v3Sums)) })

	tmp := filepath.Join(t.TempDir(), "bucks")
	if err := os.WriteFile(tmp, []byte("old"), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}
	repl := &fakeReplacer{path: tmp}
	u := updater.New(
		updater.WithAPIBase(srv.URL),
		updater.WithHTTPClient(srv.Client()),
		updater.WithVersion("v1.0.0"),
		updater.WithReplacer(repl),
	)

	var out bytes.Buffer
	err := runUpdate(context.Background(), u, strings.NewReader("y\n"), &out, updateFlags{})
	if err == nil {
		t.Fatalf("expected update to abort when latest changed; output: %q", out.String())
	}
	if repl.seen {
		t.Fatal("must not install a release different from the approved prompt tag")
	}
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("target changed despite tag drift: %q", got)
	}
	if !strings.Contains(out.String(), "changed") {
		t.Errorf("expected clear tag-change error, got: %q", out.String())
	}
}

// TestRunUpdateNetworkErrorClean proves a network failure prints a clear message and
// returns an error (non-zero exit) — no crash.
func TestRunUpdateNetworkErrorClean(t *testing.T) {
	u := updater.New(
		updater.WithAPIBase("http://127.0.0.1:0"),
		updater.WithVersion("v1.0.0"),
	)
	var out bytes.Buffer
	err := runUpdate(context.Background(), u, strings.NewReader(""), &out, updateFlags{checkOnly: true})
	if err == nil {
		t.Fatal("expected a network error")
	}
	if !strings.Contains(out.String(), "Could not check") {
		t.Errorf("expected clear failure message, got: %q", out.String())
	}
}

// TestConfirmYes proves only explicit yes answers confirm; EOF/blank/no are safe-no.
func TestConfirmYes(t *testing.T) {
	yes := []string{"y\n", "Y\n", "yes\n", "  yes  \n"}
	no := []string{"n\n", "no\n", "\n", "", "maybe\n"}
	for _, s := range yes {
		if !confirmYes(strings.NewReader(s)) {
			t.Errorf("confirmYes(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if confirmYes(strings.NewReader(s)) {
			t.Errorf("confirmYes(%q) = true, want false", s)
		}
	}
}
