package updater

import (
	"runtime"
	"strings"
	"testing"
)

// TestNewerVersionCompare covers the four required cases: a newer release, the same
// version, an older release, and a dev current build (always offered an update).
func TestNewerVersionCompare(t *testing.T) {
	cases := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{"latest newer than current", "v1.2.0", "v1.3.0", true},
		{"latest newer minor with v stripping", "1.2.0", "v1.2.1", true},
		{"same version is not newer", "v1.2.0", "v1.2.0", false},
		{"same version no-v is not newer", "1.2.0", "1.2.0", false},
		{"older release is not newer", "v2.0.0", "v1.9.9", false},
		{"dev current always gets an update", "dev", "v0.0.1", true},
		{"empty current treated as dev", "", "v9.9.9", true},
		{"major beats minor.patch", "v1.9.9", "v2.0.0", true},
		{"unparseable latest on real current is not newer", "v1.0.0", "garbage", false},
		{"prerelease suffix ignored, equal core not newer", "v1.2.0", "v1.2.0-rc1", false},
		{"build metadata ignored", "v1.2.0+build5", "v1.2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newer(tc.current, tc.latest); got != tc.want {
				t.Errorf("newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

// TestParseSemver proves the parser handles the v-prefix, missing components, and
// rejects non-numeric cores.
func TestParseSemver(t *testing.T) {
	if v, ok := parseSemver("v1"); !ok || v.major != 1 || v.minor != 0 || v.patch != 0 {
		t.Errorf("parseSemver(v1) = %+v ok=%v, want 1.0.0 ok", v, ok)
	}
	if v, ok := parseSemver("2.5"); !ok || v.major != 2 || v.minor != 5 || v.patch != 0 {
		t.Errorf("parseSemver(2.5) = %+v ok=%v, want 2.5.0 ok", v, ok)
	}
	if _, ok := parseSemver("dev"); ok {
		t.Error("parseSemver(dev) should not parse")
	}
	if _, ok := parseSemver("1.2.3.4"); ok {
		t.Error("parseSemver with 4 components should not parse")
	}
	if _, ok := parseSemver(""); ok {
		t.Error("parseSemver empty should not parse")
	}
}

// TestVersionLine proves the version line includes the version, platform, and go
// runtime version, in a stable format.
func TestVersionLine(t *testing.T) {
	line := VersionLine("go1.26.4", "linux", "amd64")
	for _, want := range []string{Version, "linux", "amd64", "go1.26.4"} {
		if !strings.Contains(line, want) {
			t.Errorf("VersionLine missing %q: %q", want, line)
		}
	}
}

// TestAssetNameMatchesRuntime proves AssetName produces the BUCKS_<goos>_<goarch>.zip
// shape used by dist/make_bucks_zip.sh, for the running platform.
func TestAssetNameMatchesRuntime(t *testing.T) {
	got := AssetName(runtime.GOOS, runtime.GOARCH)
	want := "BUCKS_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	if got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}
}
