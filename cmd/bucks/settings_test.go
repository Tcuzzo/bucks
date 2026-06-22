package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/analyst"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// TestMain prevents command-dispatch tests from reading an operator's real Bucks
// configuration. Some commands intentionally resolve the per-user config path, so
// the package test process must provide an isolated user-config home on every OS.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bucks-command-tests-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		if err := os.Setenv(name, dir); err != nil {
			panic(err)
		}
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestUpdateAISettingsPreservesUnrelatedConfiguration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "settings-round-trip-pass"
	want := validSetupResult(t)
	want.LLM = tui.LLMOAuthGPT
	want.LLMKey = ""
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	plainBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read plain config before update: %v", err)
	}

	const newKey = "nvapi-settings-secret-12345"
	if err := updateAISettings(configPath, pass, tui.LLMNemotronFree, newKey, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("updateAISettings: %v", err)
	}
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if got.LLM != tui.LLMNemotronFree || got.LLMKey != newKey {
		t.Fatalf("AI settings = (%q, %q), want (%q, saved key)", got.LLM, got.LLMKey, tui.LLMNemotronFree)
	}
	if got.TelegramToken != want.TelegramToken || !reflect.DeepEqual(got.Brokers, want.Brokers) ||
		got.Live != want.Live || got.VoiceEnabled != want.VoiceEnabled {
		t.Fatalf("AI-only update changed unrelated setup:\n got: %+v\nwant: %+v", got, want)
	}
	plainAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read plain config after update: %v", err)
	}
	if !bytes.Equal(plainAfter, plainBefore) {
		t.Fatal("AI-only update rewrote the plaintext playbook")
	}
	for _, path := range []string{configPath, secretsPathFor(configPath)} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if bytes.Contains(b, []byte(newKey)) {
			t.Fatalf("AI key leaked as plaintext in %s", path)
		}
	}
}

func TestRuntimeChatBackendsUseSavedSetupWithoutEnvironment(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "runtime-saved-pass"
	want := validSetupResult(t)
	want.LLM = tui.LLMNemotronFree
	want.LLMKey = "nvapi-runtime-saved-key"
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	for _, name := range []string{envChatProvider, envChatBaseURL, envChatKey, envChatModel} {
		t.Setenv(name, "")
	}

	backends, err := runtimeChatBackends(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("runtimeChatBackends: %v", err)
	}
	if len(backends) != 1 || backends[0].Name() != "nvidia" {
		t.Fatalf("saved runtime backends = %v, want one NVIDIA backend", backendNames(backends))
	}
}

func TestRuntimeChatBackendsPreferExplicitEnvironmentOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "runtime-override-pass"
	want := validSetupResult(t)
	want.LLM = tui.LLMNemotronFree
	want.LLMKey = "nvapi-runtime-saved-key"
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	t.Setenv(envChatProvider, "groq")
	t.Setenv(envChatKey, "gsk-explicit-override")
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatModel, "")

	backends, err := runtimeChatBackends(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("runtimeChatBackends: %v", err)
	}
	if len(backends) != 1 || backends[0].Name() != "groq" {
		t.Fatalf("override runtime backends = %v, want one Groq backend", backendNames(backends))
	}
}

func backendNames(backends []analyst.Backend) []string {
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		names = append(names, backend.Name())
	}
	return names
}

func TestCustomAISettingsSelectCustomBackendAndLeaveDefaultUntouched(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "default.yaml")
	customPath := filepath.Join(dir, "custom.yaml")
	const pass = "two-config-passphrase"
	if secretsUserFor(defaultPath) == secretsUserFor(customPath) {
		t.Fatal("different config paths resolved to the same keyring account")
	}

	defaultSetup := validSetupResult(t)
	defaultSetup.LLM = tui.LLMNemotronFree
	defaultSetup.LLMKey = "nvapi-default-config-key"
	customSetup := validSetupResult(t)
	customSetup.LLM = tui.LLMOAuthGPT
	customSetup.LLMKey = ""
	for path, setup := range map[string]tui.SetupResult{
		defaultPath: defaultSetup,
		customPath:  customSetup,
	} {
		if err := SaveSetup(setup, path, pass, secrets.ForceFileBackend()); err != nil {
			t.Fatalf("SaveSetup(%s): %v", path, err)
		}
	}

	if err := updateAISettings(customPath, pass, tui.LLMCloudKey, "custom-cloud-key", secrets.ForceFileBackend()); err != nil {
		t.Fatalf("update custom AI settings: %v", err)
	}
	stillDefault, err := LoadSetup(defaultPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("load default setup: %v", err)
	}
	if stillDefault.LLM != defaultSetup.LLM || stillDefault.LLMKey != defaultSetup.LLMKey {
		t.Fatalf("custom update changed default AI settings: got (%q, %q)", stillDefault.LLM, stillDefault.LLMKey)
	}

	defaultBackends, err := runtimeChatBackends(defaultPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("resolve default backend: %v", err)
	}
	customBackends, err := runtimeChatBackends(customPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("resolve custom backend: %v", err)
	}
	if len(defaultBackends) != 1 || defaultBackends[0].Name() != "nvidia" {
		t.Fatalf("default backends = %v, want nvidia", backendNames(defaultBackends))
	}
	if len(customBackends) != 1 || customBackends[0].Name() != "chat-cloud" {
		t.Fatalf("custom backends = %v, want chat-cloud", backendNames(customBackends))
	}
}

func TestCustomConfigSecretPathsDoNotCollideByExtension(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]string{}
	for _, name := range []string{"desk", "desk.yml", "desk.yaml"} {
		configPath := filepath.Join(dir, name)
		secretPath := secretsPathFor(configPath)
		if previous, exists := seen[secretPath]; exists {
			t.Fatalf("configs %q and %q share secret path %q", previous, name, secretPath)
		}
		seen[secretPath] = name
	}
}

func TestCustomAISettingsMigratesLegacySecretWithoutChangingIt(t *testing.T) {
	dir := t.TempDir()
	customPath := filepath.Join(dir, "custom.yaml")
	const pass = "legacy-custom-passphrase"
	legacySetup := validSetupResult(t)
	legacySetup.LLM = tui.LLMNemotronFree
	legacySetup.LLMKey = "nvapi-legacy-key"
	if err := SaveSetup(legacySetup, customPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup custom: %v", err)
	}
	if err := os.Remove(secretsPathFor(customPath)); err != nil {
		t.Fatalf("remove new namespaced secret: %v", err)
	}
	legacyStore, err := secrets.Open("", legacySecretsPathFor(customPath), pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	legacyConfig := SecretConfigFrom(legacySetup)
	if err := legacyStore.Save(legacyConfig); err != nil {
		t.Fatalf("save legacy secret: %v", err)
	}

	loaded, err := LoadSetup(customPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("load legacy custom setup: %v", err)
	}
	if loaded.LLM != legacySetup.LLM || loaded.LLMKey != legacySetup.LLMKey {
		t.Fatalf("legacy custom AI settings = (%q, %q)", loaded.LLM, loaded.LLMKey)
	}
	if err := updateAISettings(customPath, pass, tui.LLMCloudKey, "migrated-custom-key", secrets.ForceFileBackend()); err != nil {
		t.Fatalf("migrate custom AI settings: %v", err)
	}
	if _, err := os.Stat(secretsPathFor(customPath)); err != nil {
		t.Fatalf("namespaced secret was not created: %v", err)
	}
	legacyAfter, err := legacyStore.Load()
	if err != nil {
		t.Fatalf("reload legacy secret: %v", err)
	}
	if !reflect.DeepEqual(legacyAfter, legacyConfig) {
		t.Fatal("custom migration changed the legacy/default secret")
	}
}

func TestRunAISettingsUsesMaskedKeyReaderAndSaves(t *testing.T) {
	current := tui.SetupResult{LLM: tui.LLMOAuthGPT}
	var out bytes.Buffer
	var prompt string
	var savedChoice tui.LLMChoice
	var savedKey string
	readKey := func(p string) (string, error) {
		prompt = p
		return "nvapi-masked-user-key", nil
	}
	save := func(choice tui.LLMChoice, key string) error {
		savedChoice, savedKey = choice, key
		return nil
	}

	if err := runAISettings(strings.NewReader("4\n"), &out, current, readKey, save); err != nil {
		t.Fatalf("runAISettings: %v", err)
	}
	if savedChoice != tui.LLMNemotronFree || savedKey != "nvapi-masked-user-key" {
		t.Fatalf("saved (%q, %q), want Nemotron and entered key", savedChoice, savedKey)
	}
	if !strings.Contains(strings.ToLower(prompt), "key") {
		t.Fatalf("masked reader prompt = %q, want API-key prompt", prompt)
	}
	if strings.Contains(out.String(), savedKey) {
		t.Fatal("settings output echoed the API key")
	}
}

func TestRunAISettingsCancelDoesNotSave(t *testing.T) {
	called := false
	var out bytes.Buffer
	err := runAISettings(strings.NewReader("q\n"), &out, tui.SetupResult{LLM: tui.LLMOAuthGPT},
		func(string) (string, error) { t.Fatal("cancel must not prompt for a key"); return "", nil },
		func(tui.LLMChoice, string) error { called = true; return nil })
	if err != nil {
		t.Fatalf("runAISettings cancel: %v", err)
	}
	if called {
		t.Fatal("cancel saved settings")
	}
}

func TestRunDispatchesSettingsCommand(t *testing.T) {
	previous := runSettingsFn
	defer func() { runSettingsFn = previous }()
	var got []string
	runSettingsFn = func(args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	if err := run([]string{"settings", "--config", "custom.yaml"}); err != nil {
		t.Fatalf("run settings: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"--config", "custom.yaml"}) {
		t.Fatalf("settings args = %v, want --config custom.yaml", got)
	}
}

func TestSettingsCommandIsDiscoverable(t *testing.T) {
	if !knownSubcommands["settings"] {
		t.Fatal("settings is not registered as a known subcommand")
	}
	var out bytes.Buffer
	if err := runHelp(&out); err != nil {
		t.Fatalf("runHelp: %v", err)
	}
	if !strings.Contains(out.String(), "bucks settings") {
		t.Fatalf("help does not list bucks settings:\n%s", out.String())
	}
}

func TestAICommandsHonorExplicitConfigPath(t *testing.T) {
	previousChat := runChatStdioFn
	previousSummary := runSummaryStdioFn
	previousResearch := runResearchStdioFn
	previousRead := runReadStdioFn
	defer func() {
		runChatStdioFn = previousChat
		runSummaryStdioFn = previousSummary
		runResearchStdioFn = previousResearch
		runReadStdioFn = previousRead
	}()

	const custom = "custom-bucks.yaml"
	var got []string
	runChatStdioFn = func(configPath string) error {
		got = append(got, "chat="+configPath)
		return nil
	}
	runSummaryStdioFn = func(configPath string) error {
		got = append(got, "summary="+configPath)
		return nil
	}
	runResearchStdioFn = func(configPath, query string) error {
		got = append(got, "research="+configPath+":"+query)
		return nil
	}
	runReadStdioFn = func(configPath, url string) error {
		got = append(got, "read="+configPath+":"+url)
		return nil
	}

	commands := [][]string{
		{"chat", "--config", custom},
		{"summary", "--config", custom},
		{"research", "--config", custom, "acme", "earnings"},
		{"read", "--config", custom, "https://example.test"},
		{"--config", custom, "--chat"},
	}
	for _, args := range commands {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}
	want := []string{
		"chat=" + custom,
		"summary=" + custom,
		"research=" + custom + ":acme earnings",
		"read=" + custom + ":https://example.test",
		"chat=" + custom,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AI command config routing = %v, want %v", got, want)
	}
}

func TestRunSettingsHelpExitsZero(t *testing.T) {
	if err := runSettingsStdio([]string{"--help"}); err != nil {
		t.Fatalf("settings --help: %v", err)
	}
}

func TestDashboardExitRoutesSettingsAndRequestsRestart(t *testing.T) {
	model, _ := tui.NewDashboard().Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	called := false
	restart, err := handleDashboardExit(model, func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("handleDashboardExit: %v", err)
	}
	if !called || !restart {
		t.Fatalf("settings route called=%v restart=%v, want both true", called, restart)
	}
}
