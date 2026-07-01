package main

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from the test dir to the module root (the dir with go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found walking up")
		}
		dir = parent
	}
}

// TestReleaseZipHasRequiredFilesAndNoSecrets is the packaging proof. It builds the BUCKS
// binary for the host OS, stages it with the SHIPPED LICENSE + NOTICE + README + the
// guided installers exactly as make_bucks_zip.sh does, writes a real zip with the Go
// archive/zip writer (so the test does not depend on the external `zip` tool), then
// asserts:
//   - the zip contains LICENSE, NOTICE, README.md, the binary, and both installers;
//   - NO entry carries secret-looking content (the "no secrets in the zip" guarantee).
//
// It uses archive/zip rather than shelling to make_bucks_zip.sh so the assertion is
// self-contained and deterministic; make_bucks_zip.sh is exercised end-to-end separately
// (the slice's manual zip+unzip -l proof). This is the automated gate in the suite.
func TestReleaseZipHasRequiredFilesAndNoSecrets(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()

	// 1. Build the binary for the host (CGO disabled, trimpath, fixed ldflags).
	binName := "bucks"
	if runtime.GOOS == "windows" {
		binName = "bucks.exe"
	}
	binPath := filepath.Join(tmp, binName)
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X main.version=test -X main.buildDate=0",
		"-o", binPath, "./cmd/bucks")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	// 2. Stage the bundle = binary + shipped docs + installers.
	bundle := map[string]string{
		binName:       binPath,
		"LICENSE":     filepath.Join(root, "LICENSE"),
		"NOTICE":      filepath.Join(root, "NOTICE"),
		"README.md":   filepath.Join(root, "README.md"),
		"install.sh":  filepath.Join(root, "dist", "install.sh"),
		"install.ps1": filepath.Join(root, "dist", "install.ps1"),
	}

	// 3. Write a real zip with archive/zip.
	zipPath := filepath.Join(tmp, "BUCKS_test.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(zf)
	for name, src := range bundle {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s (%s): %v", name, src, err)
		}
		w, err := zw.Create("BUCKS_test/" + name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := zf.Close(); err != nil {
		t.Fatalf("close zip file: %v", err)
	}

	// 4. Re-open the zip and assert contents + no secrets.
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	required := map[string]bool{
		"BUCKS_test/" + binName:  false,
		"BUCKS_test/LICENSE":     false,
		"BUCKS_test/NOTICE":      false,
		"BUCKS_test/README.md":   false,
		"BUCKS_test/install.sh":  false,
		"BUCKS_test/install.ps1": false,
	}

	// Secret SHAPES that must never appear in a shipped text file.
	secretShapes := []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	}

	for _, f := range zr.File {
		if _, ok := required[f.Name]; ok {
			required[f.Name] = true
		}
		// Only scan text files for secrets (the binary is not text — skip it).
		if f.Name == "BUCKS_test/"+binName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			rc.Close()
			t.Fatalf("read zip entry %s: %v", f.Name, err)
		}
		rc.Close()
		for _, re := range secretShapes {
			if re.Match(buf.Bytes()) {
				t.Errorf("SECRET-SHAPED content in shipped file %s (pattern %s)", f.Name, re.String())
			}
		}
	}

	for name, present := range required {
		if !present {
			t.Errorf("release zip is MISSING required file: %s", name)
		}
	}
}

func releaseSecretShapes() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`nvapi-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	}
}

func TestReleaseSecretShapesCatchHostedLLMKeys(t *testing.T) {
	samples := []string{
		"BUCKS_CHAT_KEY=nvapi-abcdEFGH1234567890hostedLLMKey",
		"OPENAI_API_KEY=sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"OPENAI_API_KEY=sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	}

	for _, sample := range samples {
		matched := false
		for _, re := range releaseSecretShapes() {
			if re.MatchString(sample) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("release secret shapes did not catch hosted LLM key sample %q", sample)
		}
	}
}

func TestSecretScanScriptRejectsHostedLLMKeys(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	leakPath := filepath.Join(tmp, "leaked-env.txt")
	leak := strings.Join([]string{
		"BUCKS_CHAT_KEY=nvapi-abcdEFGH1234567890hostedLLMKey",
		"OPENAI_API_KEY=sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"OPENAI_API_KEY=sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	}, "\n")
	if err := os.WriteFile(leakPath, []byte(leak), 0o600); err != nil {
		t.Fatalf("write leaked fixture: %v", err)
	}

	scan := exec.Command("bash", filepath.Join(root, "dist", "secret_scan.sh"), tmp)
	out, err := scan.CombinedOutput()
	if err == nil {
		t.Fatalf("secret_scan.sh accepted hosted LLM key-shaped content; output:\n%s", out)
	}
	text := string(out)
	for _, want := range []string{"POSSIBLE SECRET", "nvapi-", "sk-proj-", "sk-AbCdEf"} {
		if !strings.Contains(text, want) {
			t.Fatalf("secret_scan.sh output missing %q; output:\n%s", want, out)
		}
	}
}

func TestSecretScanScriptAllowsHostedLLMPlaceholders(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	placeholderPath := filepath.Join(tmp, "docs.txt")
	placeholder := strings.Join([]string{
		"Paste a free nvapi-... key from build.nvidia.com.",
		"Set OPENAI_API_KEY=sk-... when using an OpenAI-compatible provider.",
	}, "\n")
	if err := os.WriteFile(placeholderPath, []byte(placeholder), 0o600); err != nil {
		t.Fatalf("write placeholder fixture: %v", err)
	}

	scan := exec.Command("bash", filepath.Join(root, "dist", "secret_scan.sh"), tmp)
	out, err := scan.CombinedOutput()
	if err != nil {
		t.Fatalf("secret_scan.sh rejected hosted LLM placeholders: %v\n%s", err, out)
	}
}

func TestGitHubWorkflowsRunSecretScan(t *testing.T) {
	root := repoRoot(t)
	workflows := map[string]struct {
		mustPrecede string
	}{
		".github/workflows/ci.yml": {},
		".github/workflows/release.yml": {
			mustPrecede: "gh release create",
		},
	}

	for workflow, rule := range workflows {
		t.Run(workflow, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, workflow))
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			text := uncommentedWorkflowText(string(data))
			scanIndex := strings.Index(text, "bash dist/secret_scan.sh .")
			if scanIndex < 0 {
				t.Fatalf("%s does not run dist/secret_scan.sh against the repo", workflow)
			}
			if rule.mustPrecede == "" {
				return
			}
			laterIndex := strings.Index(text, rule.mustPrecede)
			if laterIndex < 0 {
				t.Fatalf("%s missing expected later command %q", workflow, rule.mustPrecede)
			}
			if scanIndex > laterIndex {
				t.Fatalf("%s runs secret scan after %q; release assets must be scanned before publish", workflow, rule.mustPrecede)
			}
		})
	}
}

func uncommentedWorkflowText(text string) string {
	var active strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		active.WriteString(line)
		active.WriteByte('\n')
	}
	return active.String()
}
