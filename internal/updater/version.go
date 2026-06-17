// Package updater gives BUCKS a built-in, SAFE self-updater: an installed copy can
// update itself from the latest GitHub Release of Tcuzzo/bucks. The defining property
// is that it NEVER runs an unverified download — every downloaded release zip is
// SHA-256 verified against the release's signed SHA256SUMS asset BEFORE it is ever
// extracted or installed. A checksum mismatch ABORTS with the current binary
// untouched. That verification is the security core of this package.
//
// Everything that touches the outside world is injectable so the default test suite
// drives the FULL flow — check, download, verify, extract, atomic-replace — against
// httptest servers and a temp target file, never the real network and never the real
// running binary.
package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the build-stamped release version of this binary. It is "dev" for an
// un-stamped (e.g. `go build` / `go run`) build, and is overridden at release time
// with ldflags, e.g.:
//
//	go build -ldflags "-X 'bucks/internal/updater.Version=v1.2.0'" ./cmd/bucks
//
// dist/make_bucks_zip.sh passes exactly this so shipped binaries report their tag.
var Version = "dev"

// semver holds the numeric major.minor.patch parsed from a tag. Pre-release/build
// metadata (a "-rc1" or "+meta" suffix) is intentionally ignored for the comparison:
// the updater only needs "is the release tag a higher version than what I am".
type semver struct {
	major, minor, patch int
}

// parseSemver strips a single leading "v" and any pre-release/build suffix, then
// parses the dotted numeric core. Missing minor/patch default to 0 (so "v1" == 1.0.0).
// A non-numeric core returns ok=false; the caller treats that as "cannot compare".
func parseSemver(tag string) (semver, bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return semver{}, false
	}
	// Drop build metadata ("+...") then pre-release ("-...") — neither affects the
	// numeric precedence we use here.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semver{}, false
	}
	out := semver{}
	dst := []*int{&out.major, &out.minor, &out.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return semver{}, false
		}
		*dst[i] = n
	}
	return out, true
}

// compareSemver returns -1 if a<b, 0 if equal, +1 if a>b.
func compareSemver(a, b semver) int {
	switch {
	case a.major != b.major:
		return cmpInt(a.major, b.major)
	case a.minor != b.minor:
		return cmpInt(a.minor, b.minor)
	default:
		return cmpInt(a.patch, b.patch)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// isDevVersion reports whether v is an un-stamped development build. Such a build has
// no real version, so the updater always offers the latest release as an update (and
// the caller notes it is replacing a dev build).
func isDevVersion(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return s == "" || s == "dev"
}

// newer reports whether the release tag `latest` is a NEWER version than `current`.
//   - If current is a dev build, ANY parseable latest counts as newer (you are on an
//     un-versioned build; the release is the thing to move to).
//   - Otherwise it is a numeric major.minor.patch comparison (leading "v" stripped).
//   - If either side cannot be parsed (and current is not dev), it reports false — we
//     do not claim "newer" we cannot prove, so a weird tag never triggers a download.
func newer(current, latest string) bool {
	lv, lok := parseSemver(latest)
	if isDevVersion(current) {
		return lok
	}
	cv, cok := parseSemver(current)
	if !cok || !lok {
		return false
	}
	return compareSemver(lv, cv) > 0
}

// describeCurrent renders the current version for user-facing messages, making the
// dev case explicit.
func describeCurrent(v string) string {
	if isDevVersion(v) {
		return "dev (un-versioned build)"
	}
	return v
}

// VersionLine is the human-readable string printed by `bucks version`. goVersion,
// goos and goarch are injected so the line is fully testable offline.
func VersionLine(goVersion, goos, goarch string) string {
	return fmt.Sprintf("bucks %s (%s/%s, %s)", Version, goos, goarch, goVersion)
}
