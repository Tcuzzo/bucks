package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeReplacer is a test Replacer that points at a temp file (NEVER the real test
// binary). It records what was installed so tests assert the new bytes landed.
type fakeReplacer struct {
	path        string
	exeErr      error
	replaceErr  error
	replaceSeen bool
}

func (f *fakeReplacer) ExePath() (string, error) {
	if f.exeErr != nil {
		return "", f.exeErr
	}
	return f.path, nil
}

func (f *fakeReplacer) Replace(exePath string, newBinary []byte) error {
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.replaceSeen = true
	return os.WriteFile(exePath, newBinary, 0o755)
}

// buildZip makes a real in-memory zip containing a top-level dir plus the named
// binary with the given contents — mirroring make_bucks_zip.sh's layout.
func buildZip(t *testing.T, binName string, binData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A few sibling files like the real zip, to prove extraction picks the binary.
	for name, data := range map[string][]byte{
		"BUCKS_test/LICENSE":   []byte("license"),
		"BUCKS_test/README.md": []byte("readme"),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	w, err := zw.Create("BUCKS_test/" + binName)
	if err != nil {
		t.Fatalf("zip create bin: %v", err)
	}
	if _, err := w.Write(binData); err != nil {
		t.Fatalf("zip write bin: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer stands up an httptest server that serves the latest-release JSON, the
// zip asset, and the SHA256SUMS asset. The caller passes the bytes to serve for each.
// Returns the server (caller closes) and the API base URL to inject.
func releaseServer(t *testing.T, tag, goos, goarch string, zipBytes, sumsBytes []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	zipName := AssetName(goos, goarch)
	zipURL := srv.URL + "/dl/" + zipName
	sumsURL := srv.URL + "/dl/" + sumsAssetName

	mux.HandleFunc("/repos/Tcuzzo/bucks/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":%q,"browser_download_url":%q}]}`,
			tag, zipName, zipURL, sumsAssetName, sumsURL)
	})
	mux.HandleFunc("/dl/"+zipName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	})
	mux.HandleFunc("/dl/"+sumsAssetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(sumsBytes)
	})
	return srv
}

// TestCheckLatestParsesReleaseJSON proves CheckLatest reads tag + assets from a mock
// GitHub releases/latest response and computes "newer" correctly.
func TestCheckLatestParsesReleaseJSON(t *testing.T) {
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v2.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
	)
	rel, err := u.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel.Tag != "v2.0.0" {
		t.Errorf("tag = %q, want v2.0.0", rel.Tag)
	}
	if !rel.IsNewer {
		t.Error("v2.0.0 should be newer than v1.0.0")
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("want 2 assets, got %d", len(rel.Assets))
	}
	if _, ok := findAsset(rel.Assets, AssetName("linux", "amd64")); !ok {
		t.Error("platform zip asset missing")
	}
	if _, ok := findAsset(rel.Assets, sumsAssetName); !ok {
		t.Error("SHA256SUMS asset missing")
	}
}

// TestAssetSelectionMissingPlatform proves Update errors clearly when there is no
// asset for the running platform.
func TestAssetSelectionMissingPlatform(t *testing.T) {
	// Build a release that has assets for linux/amd64 only, but ask as darwin/arm64.
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v2.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("darwin", "arm64"), // no asset for this
		WithReplacer(&fakeReplacer{path: tmp}),
	)
	_, err := u.Update(context.Background(), Options{})
	if !errors.Is(err, ErrNoAssetForPlatform) {
		t.Fatalf("want ErrNoAssetForPlatform, got %v", err)
	}
}

// TestUpdateVerifiesAndInstalls is the happy path: a real zip + a MATCHING SHA256SUMS
// is served, Update verifies the checksum, extracts the binary, and the injected
// target file receives the EXACT new binary bytes.
func TestUpdateVerifiesAndInstalls(t *testing.T) {
	newBin := []byte("\x7fELF this-is-the-new-bucks-binary-v2")
	zipBytes := buildZip(t, "bucks", newBin)
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v2.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	if err := os.WriteFile(tmp, []byte("OLD BINARY v1"), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}
	repl := &fakeReplacer{path: tmp}

	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
		WithReplacer(repl),
	)
	res, err := u.Update(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.Updated {
		t.Fatal("Update reported not updated")
	}
	if !repl.replaceSeen {
		t.Fatal("Replace was never called")
	}
	if res.NewVersion != "v2.0.0" {
		t.Errorf("NewVersion = %q, want v2.0.0", res.NewVersion)
	}
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if !bytes.Equal(got, newBin) {
		t.Fatalf("installed bytes mismatch:\n got %q\nwant %q", got, newBin)
	}
}

// TestUpdateAbortsOnChecksumMismatch is the LOAD-BEARING security test. A CORRUPTED
// zip is served while SHA256SUMS records the checksum of the GOOD zip. Update must
// ABORT with ErrChecksumMismatch and leave the target file UNTOUCHED (no extraction,
// no install).
func TestUpdateAbortsOnChecksumMismatch(t *testing.T) {
	goodBin := []byte("\x7fELF the-legitimate-binary")
	goodZip := buildZip(t, "bucks", goodBin)
	// SHA256SUMS lists the GOOD zip's checksum...
	sums := sha256Hex(goodZip) + "  " + AssetName("linux", "amd64") + "\n"
	// ...but the server serves a DIFFERENT (corrupted / tampered) zip.
	badZip := buildZip(t, "bucks", []byte("\x7fELF MALICIOUS payload swapped in transit"))
	if bytes.Equal(goodZip, badZip) {
		t.Fatal("test setup: good and bad zips are identical")
	}
	srv := releaseServer(t, "v2.0.0", "linux", "amd64", badZip, []byte(sums))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	const original = "OLD BINARY v1 — must survive"
	if err := os.WriteFile(tmp, []byte(original), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}
	repl := &fakeReplacer{path: tmp}

	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
		WithReplacer(repl),
	)
	res, err := u.Update(context.Background(), Options{})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got err=%v res=%+v", err, res)
	}
	if res.Updated {
		t.Fatal("Update reported updated on a checksum mismatch")
	}
	if repl.replaceSeen {
		t.Fatal("Replace was called despite a checksum mismatch — install must NOT happen")
	}
	// The on-disk target must be byte-for-byte the original.
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read target after abort: %v", err)
	}
	if string(got) != original {
		t.Fatalf("target was modified on checksum abort: got %q", got)
	}
}

// TestUpdateAbortsWhenSumsHasNoEntry proves a SHA256SUMS that omits the platform zip
// is treated as a verification failure (we never install an unverifiable download).
func TestUpdateAbortsWhenSumsHasNoEntry(t *testing.T) {
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	// SHA256SUMS lists a DIFFERENT filename only — no entry for our zip.
	sums := sha256Hex(zipBytes) + "  BUCKS_someotheros_arch.zip\n"
	srv := releaseServer(t, "v2.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	repl := &fakeReplacer{path: tmp}
	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
		WithReplacer(repl),
	)
	_, err := u.Update(context.Background(), Options{})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch for missing sums entry, got %v", err)
	}
	if repl.replaceSeen {
		t.Fatal("Replace called despite no checksum entry")
	}
}

// TestUpdateNoChecksumsAsset proves a release missing SHA256SUMS aborts (refuse to
// install something we cannot verify).
func TestUpdateNoChecksumsAsset(t *testing.T) {
	zipName := AssetName("linux", "amd64")
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// Release JSON advertises ONLY the zip, no SHA256SUMS.
	mux.HandleFunc("/repos/Tcuzzo/bucks/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v2.0.0","assets":[{"name":%q,"browser_download_url":%q}]}`,
			zipName, srv.URL+"/dl/"+zipName)
	})
	mux.HandleFunc("/dl/"+zipName, func(w http.ResponseWriter, r *http.Request) { w.Write(zipBytes) })

	tmp := filepath.Join(t.TempDir(), "bucks")
	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
		WithReplacer(&fakeReplacer{path: tmp}),
	)
	_, err := u.Update(context.Background(), Options{})
	if !errors.Is(err, ErrNoChecksums) {
		t.Fatalf("want ErrNoChecksums, got %v", err)
	}
}

// TestUpdateAlreadyUpToDate proves that when the latest is NOT newer (and not forced),
// Update does nothing: no download, no replace, a clear "up to date" message.
func TestUpdateAlreadyUpToDate(t *testing.T) {
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v1.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	repl := &fakeReplacer{path: filepath.Join(t.TempDir(), "bucks")}
	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"), // same as latest
		WithPlatform("linux", "amd64"),
		WithReplacer(repl),
	)
	res, err := u.Update(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Updated {
		t.Error("Update should be a no-op when already up to date")
	}
	if repl.replaceSeen {
		t.Error("Replace must not be called when up to date")
	}
	if !strings.Contains(res.Message, "up to date") {
		t.Errorf("message = %q, want 'up to date'", res.Message)
	}
}

// TestUpdateForceReinstallsSameVersion proves --force installs even when not newer
// (and still verifies the checksum).
func TestUpdateForceReinstallsSameVersion(t *testing.T) {
	newBin := []byte("reinstalled-bytes")
	zipBytes := buildZip(t, "bucks", newBin)
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v1.0.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "bucks")
	os.WriteFile(tmp, []byte("old"), 0o755)
	repl := &fakeReplacer{path: tmp}
	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
		WithReplacer(repl),
	)
	res, err := u.Update(context.Background(), Options{Force: true})
	if err != nil {
		t.Fatalf("forced Update: %v", err)
	}
	if !res.Updated {
		t.Fatal("forced Update should install")
	}
	got, _ := os.ReadFile(tmp)
	if !bytes.Equal(got, newBin) {
		t.Fatalf("forced install bytes mismatch: got %q", got)
	}
}

// TestCheckLatestDevBuild proves a dev current build is always offered the latest as
// an update, with the dev flag set.
func TestCheckLatestDevBuild(t *testing.T) {
	zipBytes := buildZip(t, "bucks", []byte("binary"))
	sums := sha256Hex(zipBytes) + "  " + AssetName("linux", "amd64") + "\n"
	srv := releaseServer(t, "v0.1.0", "linux", "amd64", zipBytes, []byte(sums))
	defer srv.Close()

	u := New(
		WithAPIBase(srv.URL),
		WithHTTPClient(srv.Client()),
		WithVersion("dev"),
		WithPlatform("linux", "amd64"),
	)
	rel, err := u.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if !rel.IsNewer {
		t.Error("dev build should always have an update available")
	}
	if !rel.IsDevCur {
		t.Error("IsDevCur should be true for a dev build")
	}
}

// TestCheckLatestNetworkError proves a failed network call surfaces a clear error
// rather than crashing or claiming a (fake) result.
func TestCheckLatestNetworkError(t *testing.T) {
	u := New(
		WithAPIBase("http://127.0.0.1:0"), // nothing is listening
		WithVersion("v1.0.0"),
		WithPlatform("linux", "amd64"),
	)
	if _, err := u.CheckLatest(context.Background()); err == nil {
		t.Fatal("expected a network error, got nil")
	}
}

// TestLookupChecksum proves the SHA256SUMS parser handles the sha256sum format,
// binary-mode markers, blank/comment lines, and path components.
func TestLookupChecksum(t *testing.T) {
	body := strings.Join([]string{
		"# a comment line",
		"",
		"deadbeef  BUCKS_linux_amd64.zip",
		"cafef00d *BUCKS_windows_amd64.zip", // binary-mode marker
		"0badc0de  dist/out/BUCKS_darwin_arm64.zip",
	}, "\n")

	if sum, ok := lookupChecksum(body, "BUCKS_linux_amd64.zip"); !ok || sum != "deadbeef" {
		t.Errorf("linux lookup = %q ok=%v, want deadbeef", sum, ok)
	}
	if sum, ok := lookupChecksum(body, "BUCKS_windows_amd64.zip"); !ok || sum != "cafef00d" {
		t.Errorf("windows (binary marker) lookup = %q ok=%v, want cafef00d", sum, ok)
	}
	if sum, ok := lookupChecksum(body, "BUCKS_darwin_arm64.zip"); !ok || sum != "0badc0de" {
		t.Errorf("darwin (path) lookup = %q ok=%v, want 0badc0de", sum, ok)
	}
	if _, ok := lookupChecksum(body, "BUCKS_nope_arch.zip"); ok {
		t.Error("missing entry should report ok=false")
	}
}

// TestExtractBinaryPicksRightName proves extraction picks bucks vs bucks.exe by goos
// and errors when the binary is absent.
func TestExtractBinaryPicksRightName(t *testing.T) {
	unixZip := buildZip(t, "bucks", []byte("unix-bin"))
	if got, err := extractBinary(unixZip, "linux"); err != nil || string(got) != "unix-bin" {
		t.Errorf("unix extract = %q err=%v, want unix-bin", got, err)
	}
	winZip := buildZip(t, "bucks.exe", []byte("win-bin"))
	if got, err := extractBinary(winZip, "windows"); err != nil || string(got) != "win-bin" {
		t.Errorf("windows extract = %q err=%v, want win-bin", got, err)
	}
	// A zip with the wrong-OS binary name must error for this platform.
	if _, err := extractBinary(unixZip, "windows"); err == nil {
		t.Error("extracting bucks.exe from a unix-only zip should error")
	}
}

// TestOSReplacerAtomicReplace exercises the REAL osReplacer.Replace against a temp
// file (not the test binary): the new bytes must land atomically with 0755 perms.
func TestOSReplacerAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bucks")
	if err := os.WriteFile(target, []byte("old-bytes"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	newBytes := []byte("brand-new-binary-bytes")
	if err := (osReplacer{}).Replace(target, newBytes); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, newBytes) {
		t.Fatalf("replaced bytes = %q, want %q", got, newBytes)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows does not expose Unix execute bits through os.FileMode; os.Chmod accepts
	// 0755 but Stat reports the writable file as 0666. Keep the permission assertion
	// on platforms where those bits are meaningful while preserving the replacement,
	// content, and cleanup checks on Windows.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("perm = %v, want 0755", info.Mode().Perm())
	}
	// No temp leftovers in the dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bucks-update-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
