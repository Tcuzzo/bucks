package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runSecretScan runs dist/secret_scan.sh against target and returns its exit code and
// combined output. Exit 0 = clean, exit 1 = a possible secret was found.
func runSecretScan(t *testing.T, scriptPath, target string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", scriptPath, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		t.Fatalf("run secret_scan.sh: %v\n%s", err, out)
	}
	return 0, string(out)
}

// writeScanFile stages a single text file with the given content inside a fresh temp dir
// and returns that dir (the scan target). The filename is deliberately NOT one the scanner
// excludes (no _test.go / .age / SHA256SUMS), so its content is actually scanned.
func writeScanFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.conf"), []byte(content), 0o644); err != nil {
		t.Fatalf("write scan file: %v", err)
	}
	return dir
}

// TestSecretScanFlagsPrivateLANHostIPs proves the secret scanner (the CI/ship gate) now
// catches a leaked private LAN host address (RFC1918 IP), while NOT tripping on loopback,
// 0.0.0.0, the RFC1918 network/boundary base addresses, the RFC5737 doc ranges, or
// out-of-range/invalid octets — and that the exit-code contract (0 = clean, 1 = possible
// secret) is intact. It exercises the real dist/secret_scan.sh so the guarantee is proven
// end to end, not reimplemented.
func TestSecretScanFlagsPrivateLANHostIPs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secret_scan.sh is a bash script; CI runs it on ubuntu")
	}
	root := repoRoot(t)
	scriptPath := filepath.Join(root, "dist", "secret_scan.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("secret_scan.sh not found at %s: %v", scriptPath, err)
	}

	// A leaked private LAN host IP MUST be flagged (exit 1), across all three RFC1918
	// blocks and both edges of the 172.16.0.0/12 range.
	leaked := []string{
		"10.1.2.3",       // scan-ok: fixture
		"172.16.0.1",     // just past the 172 base, inside 172.16/12 (scan-ok: fixture)
		"172.31.255.255", // top of 172.16/12 (scan-ok: fixture)
		"192.168.7.42",   // scan-ok: fixture
	}
	for _, ip := range leaked {
		ip := ip
		t.Run("flags/"+ip, func(t *testing.T) {
			dir := writeScanFile(t, "internal_host = "+ip+"\n")
			code, out := runSecretScan(t, scriptPath, dir)
			if code != 1 {
				t.Fatalf("expected exit 1 (secret found) for leaked IP %s, got %d\n%s", ip, code, out)
			}
			if !strings.Contains(out, ip) {
				t.Fatalf("scan flagged but did not report the offending IP %s\n%s", ip, out)
			}
		})
	}

	// A boundary base address on the SAME line as a real host IP must NOT mask the leak
	// (the exclusion is per-match, not per-line). The real IP is still reported.
	t.Run("boundaryBaseDoesNotMaskRealIP", func(t *testing.T) {
		dir := writeScanFile(t, "network = 10.0.0.0 host = 10.1.2.3\n") // scan-ok: fixture
		code, out := runSecretScan(t, scriptPath, dir)
		if code != 1 {
			t.Fatalf("expected exit 1: a real host IP sharing a line with the base was masked\n%s", out)
		}
		if !strings.Contains(out, "10.1.2.3") { // scan-ok: fixture
			t.Fatalf("real host IP 10.1.2.3 was not reported (masked by the base address)\n%s", out) // scan-ok: fixture
		}
	})

	// Loopback, unspecified, the RFC1918 boundary/network bases, the RFC5737 doc ranges,
	// public 172.x outside 172.16/12, invalid octets, and the semver string must NOT be
	// flagged (exit 0) — none is a private-host leak.
	notLeaked := map[string]string{
		"loopback":         "127.0.0.1",
		"unspecified":      "0.0.0.0",
		"base-10":          "10.0.0.0",
		"base-172":         "172.16.0.0",
		"base-192":         "192.168.0.0",
		"rfc5737-a":        "192.0.2.1",
		"rfc5737-b":        "198.51.100.1",
		"rfc5737-c":        "203.0.113.1",
		"public-172-below": "172.15.0.1", // 172.15 is public, not RFC1918
		"public-172-above": "172.32.0.1", // 172.32 is public, not RFC1918
		"invalid-octet":    "192.168.999.999",
		"invalid-octet-2":  "10.300.1.1",
		"semver-1.2.3.4":   "1.2.3.4",
	}
	for name, ip := range notLeaked {
		name, ip := name, ip
		t.Run("clean/"+name, func(t *testing.T) {
			dir := writeScanFile(t, "addr = "+ip+"\n")
			code, out := runSecretScan(t, scriptPath, dir)
			if code != 0 {
				t.Fatalf("expected exit 0 (clean) for %s (%s), got %d\n%s", name, ip, code, out)
			}
		})
	}

	// The real bucks tree itself must still pass (no false positive on real content).
	t.Run("realRepoPasses", func(t *testing.T) {
		code, out := runSecretScan(t, scriptPath, root)
		if code != 0 {
			t.Fatalf("secret_scan.sh flagged the clean bucks tree (false positive), exit %d\n%s", code, out)
		}
	})
}
