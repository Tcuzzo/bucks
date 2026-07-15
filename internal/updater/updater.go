package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultRepo is the GitHub "owner/name" whose Releases BUCKS updates itself from.
const DefaultRepo = "Tcuzzo/bucks"

// DefaultAPIBase is the GitHub REST API base. It is overridable so the default test
// suite points the Updater at an httptest server — no live network in tests.
const DefaultAPIBase = "https://api.github.com"

const (
	// defaultTimeout bounds a single HTTP GET end-to-end.
	defaultTimeout = 30 * time.Second
	// maxJSONBytes caps the releases JSON we will read (the API response is small).
	maxJSONBytes = 4 << 20 // 4 MiB
	// maxSumsBytes caps the SHA256SUMS asset (a handful of lines).
	maxSumsBytes = 1 << 20 // 1 MiB
	// maxZipBytes caps the release zip download so a hostile/huge asset cannot
	// exhaust memory or disk. BUCKS zips are a few MB; 256 MiB is a generous ceiling.
	maxZipBytes = 256 << 20 // 256 MiB
	// sumsAssetName is the checksums asset published alongside the zips.
	sumsAssetName = "SHA256SUMS"
	// userAgent identifies the updater honestly; the GitHub API requires a UA.
	userAgent = "bucks-update/1.0 (+read-only; net/http)"
)

// ErrChecksumMismatch is returned when a downloaded zip's SHA-256 does not match the
// entry in SHA256SUMS. It is the security sentinel: on this error NOTHING is extracted
// and the installed binary is left untouched.
var ErrChecksumMismatch = errors.New("updater: downloaded zip failed checksum verification")

// ErrNoAssetForPlatform is returned when the latest release has no
// BUCKS_<goos>_<goarch>.zip asset for the running platform.
var ErrNoAssetForPlatform = errors.New("updater: no release asset for this platform")

// ErrNoChecksums is returned when the release is missing its SHA256SUMS asset — we
// refuse to install a download we cannot verify.
var ErrNoChecksums = errors.New("updater: release has no SHA256SUMS asset to verify against")

// Asset is one downloadable file attached to a GitHub release.
type Asset struct {
	Name string
	URL  string
}

// Release is the parsed subset of a GitHub release we use: its tag, its assets, and
// whether it is newer than the running build.
type Release struct {
	Tag      string
	Assets   []Asset
	IsNewer  bool
	IsDevCur bool // the CURRENT build is a dev (un-versioned) build
}

// AssetName returns the platform release-zip filename for a goos/goarch, matching
// dist/make_bucks_zip.sh (BUCKS_<goos>_<goarch>.zip).
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("BUCKS_%s_%s.zip", goos, goarch)
}

// Replacer performs the final, atomic swap of the running executable. It is an
// interface so tests inject a target that points at a temp file (never the real test
// binary). osReplacer is the production implementation.
type Replacer interface {
	// ExePath returns the absolute path of the executable to replace.
	ExePath() (string, error)
	// Replace atomically installs newBinary (already the new bytes) at exePath. It
	// must be atomic on the same filesystem (write-temp-then-rename).
	Replace(exePath string, newBinary []byte) error
}

// Updater checks for and applies updates. All outside I/O is injectable.
type Updater struct {
	http     *http.Client
	repo     string
	apiBase  string
	version  string
	goos     string
	goarch   string
	replacer Replacer
}

// Option configures an Updater.
type Option func(*Updater)

// WithHTTPClient injects the HTTP client (e.g. an httptest client). Nil is ignored.
func WithHTTPClient(hc *http.Client) Option {
	return func(u *Updater) {
		if hc != nil {
			u.http = hc
		}
	}
}

// WithRepo overrides the owner/name repo. Empty is ignored.
func WithRepo(repo string) Option {
	return func(u *Updater) {
		if strings.TrimSpace(repo) != "" {
			u.repo = repo
		}
	}
}

// WithAPIBase overrides the GitHub API base URL (for tests). Empty is ignored.
func WithAPIBase(base string) Option {
	return func(u *Updater) {
		if strings.TrimSpace(base) != "" {
			u.apiBase = strings.TrimRight(base, "/")
		}
	}
}

// WithVersion overrides the current version (defaults to package Version). Empty is
// ignored.
func WithVersion(v string) Option {
	return func(u *Updater) {
		if strings.TrimSpace(v) != "" {
			u.version = v
		}
	}
}

// WithPlatform overrides the goos/goarch used to pick the asset (for tests). Empty
// values are ignored.
func WithPlatform(goos, goarch string) Option {
	return func(u *Updater) {
		if strings.TrimSpace(goos) != "" {
			u.goos = goos
		}
		if strings.TrimSpace(goarch) != "" {
			u.goarch = goarch
		}
	}
}

// WithReplacer injects the executable replacer (tests point it at a temp file). Nil
// is ignored.
func WithReplacer(r Replacer) Option {
	return func(u *Updater) {
		if r != nil {
			u.replacer = r
		}
	}
}

// New builds an Updater with production defaults: the real repo, the real GitHub API,
// the build-stamped Version, the running platform, a bounded HTTP client, and the
// real os-level executable replacer.
func New(opts ...Option) *Updater {
	u := &Updater{
		http:     &http.Client{Timeout: defaultTimeout},
		repo:     DefaultRepo,
		apiBase:  DefaultAPIBase,
		version:  Version,
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
		replacer: osReplacer{},
	}
	for _, o := range opts {
		o(u)
	}
	if u.http == nil {
		u.http = &http.Client{Timeout: defaultTimeout}
	}
	if u.replacer == nil {
		u.replacer = osReplacer{}
	}
	return u
}

// CurrentVersion returns the version this Updater treats as installed.
func (u *Updater) CurrentVersion() string { return u.version }

// get issues a bounded read-only GET and returns the body bytes. It refuses non-2xx
// with a clear error (so a 404 release or a rate-limit is honest, never silently
// treated as content).
func (u *Updater) get(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("updater: build GET %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("updater: GET %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max))
	if err != nil {
		return nil, fmt.Errorf("updater: read %s: %w", rawURL, err)
	}
	return body, nil
}

// ghRelease mirrors the subset of the GitHub releases/latest JSON we consume.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// CheckLatest fetches the latest release and reports its tag, assets, and whether it
// is newer than the running build. It makes NO change — it only reads.
func (u *Updater) CheckLatest(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase, u.repo)
	body, err := u.get(ctx, url, maxJSONBytes)
	if err != nil {
		return Release{}, err
	}
	var gr ghRelease
	if err := json.Unmarshal(body, &gr); err != nil {
		return Release{}, fmt.Errorf("updater: parse release JSON: %w", err)
	}
	rel := Release{
		Tag:      strings.TrimSpace(gr.TagName),
		IsNewer:  newer(u.version, gr.TagName),
		IsDevCur: isDevVersion(u.version),
	}
	for _, a := range gr.Assets {
		name := strings.TrimSpace(a.Name)
		if name == "" || strings.TrimSpace(a.URL) == "" {
			continue
		}
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: strings.TrimSpace(a.URL)})
	}
	return rel, nil
}

// findAsset returns the asset whose name exactly matches want, or false.
func findAsset(assets []Asset, want string) (Asset, bool) {
	for _, a := range assets {
		if a.Name == want {
			return a, true
		}
	}
	return Asset{}, false
}

// Options controls an Update run.
type Options struct {
	// Force performs the install even if the latest is not newer (a reinstall).
	Force bool
	// ExpectedTag, when set, is the release tag the caller already showed the user.
	// Update aborts if the rechecked latest tag differs, so consent stays tied to
	// the artifact being installed.
	ExpectedTag string
}

// Result reports the outcome of an Update.
type Result struct {
	Updated    bool   // an install actually happened
	OldVersion string // the version before (the running build)
	NewVersion string // the release tag installed (or that would be installed)
	ExePath    string // the executable path that was (or would be) replaced
	Message    string // a plain-English summary for the user
}

// Update performs the full safe update: check, locate the platform asset + checksums,
// download both (bounded), VERIFY the zip's SHA-256 against SHA256SUMS, and only on a
// match extract the binary and atomically replace the running executable. A checksum
// mismatch returns ErrChecksumMismatch with NOTHING installed.
func (u *Updater) Update(ctx context.Context, opts Options) (Result, error) {
	rel, err := u.CheckLatest(ctx)
	if err != nil {
		return Result{}, err
	}
	if opts.ExpectedTag != "" && rel.Tag != opts.ExpectedTag {
		return Result{}, fmt.Errorf("updater: latest release changed from %s to %s; aborting", opts.ExpectedTag, rel.Tag)
	}
	res := Result{
		OldVersion: describeCurrent(u.version),
		NewVersion: rel.Tag,
	}

	if !rel.IsNewer && !opts.Force {
		res.Message = fmt.Sprintf("already up to date (%s); latest release is %s", describeCurrent(u.version), rel.Tag)
		return res, nil
	}

	// Resolve the path we will replace up front, so a bad/unresolvable exe path fails
	// BEFORE any download.
	exePath, err := u.replacer.ExePath()
	if err != nil {
		return res, fmt.Errorf("updater: locate current executable: %w", err)
	}
	res.ExePath = exePath

	// Locate the platform zip + the checksums asset.
	wantZip := AssetName(u.goos, u.goarch)
	zipAsset, ok := findAsset(rel.Assets, wantZip)
	if !ok {
		return res, fmt.Errorf("%w: looked for %s in release %s", ErrNoAssetForPlatform, wantZip, rel.Tag)
	}
	sumsAsset, ok := findAsset(rel.Assets, sumsAssetName)
	if !ok {
		return res, fmt.Errorf("%w (release %s)", ErrNoChecksums, rel.Tag)
	}

	// Download both, bounded.
	zipBytes, err := u.get(ctx, zipAsset.URL, maxZipBytes)
	if err != nil {
		return res, fmt.Errorf("updater: download %s: %w", wantZip, err)
	}
	sumsBytes, err := u.get(ctx, sumsAsset.URL, maxSumsBytes)
	if err != nil {
		return res, fmt.Errorf("updater: download %s: %w", sumsAssetName, err)
	}

	// VERIFY — the security core. Compute the SHA-256 of the downloaded zip and
	// compare it to the recorded checksum for this exact filename. Mismatch ABORTS
	// before any extraction or install.
	want, ok := lookupChecksum(string(sumsBytes), wantZip)
	if !ok {
		return res, fmt.Errorf("%w: %s has no entry in %s", ErrChecksumMismatch, wantZip, sumsAssetName)
	}
	got := sha256.Sum256(zipBytes)
	gotHex := hex.EncodeToString(got[:])
	if !strings.EqualFold(gotHex, want) {
		return res, fmt.Errorf("%w: %s expected %s got %s", ErrChecksumMismatch, wantZip, want, gotHex)
	}

	// Only now — after a verified match — extract the binary from the zip.
	bin, err := extractBinary(zipBytes, u.goos)
	if err != nil {
		return res, err
	}

	// Atomically replace the running executable with the verified new binary.
	if err := u.replacer.Replace(exePath, bin); err != nil {
		return res, fmt.Errorf("updater: install new binary: %w", err)
	}

	res.Updated = true
	res.Message = fmt.Sprintf("updated %s -> %s; re-run bucks to use the new version", describeCurrent(u.version), rel.Tag)
	return res, nil
}

// lookupChecksum parses a SHA256SUMS body ("<hexsum>  <filename>" per line, the
// sha256sum/shasum format) and returns the hex checksum recorded for want. The
// filename may carry a leading "*" (binary-mode marker) or path components; we match
// on the base name. Returns ok=false if there is no entry for want.
func lookupChecksum(sums, want string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.TrimSpace(fields[0])
		name := strings.TrimSpace(strings.Join(fields[1:], " "))
		name = strings.TrimPrefix(name, "*") // binary-mode marker
		if filepath.Base(name) == want {
			return sum, true
		}
	}
	return "", false
}

// extractBinary pulls the bucks/bucks.exe binary out of a verified release zip and
// returns its bytes. The expected name is "bucks" (unix) or "bucks.exe" (windows);
// matching is on the base name so a zip with a top-level dir still resolves.
func extractBinary(zipBytes []byte, goos string) ([]byte, error) {
	want := "bucks"
	if goos == "windows" {
		want = "bucks.exe"
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("updater: open verified zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("updater: open %s in zip: %w", f.Name, err)
		}
		// Bound the extracted binary the same way the zip download is bounded.
		data, err := io.ReadAll(io.LimitReader(rc, maxZipBytes))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("updater: read %s from zip: %w", f.Name, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("updater: %s in zip is empty", f.Name)
		}
		return data, nil
	}
	return nil, fmt.Errorf("updater: release zip has no %s binary", want)
}

// osReplacer is the production Replacer: it resolves os.Executable() (following
// symlinks) and atomically swaps it on the SAME filesystem.
type osReplacer struct{}

// ExePath returns the resolved absolute path of the running executable.
func (osReplacer) ExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so we replace the real file, not a link in PATH.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// Replace atomically installs newBinary at exePath. It writes a temp file in the SAME
// directory (so os.Rename is an atomic same-filesystem move), sets 0755, then renames
// over the target. On Windows a running .exe cannot be overwritten directly, so the
// current exe is first renamed aside to ".old" (Windows allows renaming a running
// exe); a stale ".old" is removed best-effort.
func (osReplacer) Replace(exePath string, newBinary []byte) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".bucks-update-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// On any failure from here, do not leave the temp behind.
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}

	if runtime.GOOS == "windows" {
		old := exePath + ".old"
		_ = os.Remove(old) // clear a leftover from a prior update, best-effort
		if err := os.Rename(exePath, old); err != nil {
			cleanup()
			return fmt.Errorf("rename running exe aside: %w", err)
		}
		if err := os.Rename(tmpName, exePath); err != nil {
			// Try to restore the original so we never leave the user with no binary.
			_ = os.Rename(old, exePath)
			cleanup()
			return fmt.Errorf("install new exe: %w", err)
		}
		_ = os.Remove(old) // best-effort; safe to leave if the running exe is locked
		return nil
	}

	if err := os.Rename(tmpName, exePath); err != nil {
		cleanup()
		return fmt.Errorf("atomic rename onto %s: %w", exePath, err)
	}
	return nil
}
