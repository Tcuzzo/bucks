package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceDocsDescribePairingAsDefault(t *testing.T) {
	root := repoRoot(t)
	docs := map[string]string{
		"README.md":                     filepath.Join(root, "README.md"),
		"dist/bucks.service":            filepath.Join(root, "dist", "bucks.service"),
		"dist/bucks-service-windows.md": filepath.Join(root, "dist", "bucks-service-windows.md"),
	}

	for name, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(data)
		lower := strings.ToLower(text)
		for _, phrase := range []string{
			"required for commands to work",
			"bucks ignores every command",
			"no one until you tell it who you are",
			"give the service the passphrase + chat id",
			"000000000",
		} {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s still treats BUCKS_TELEGRAM_CHAT_ID as mandatory via %q", name, phrase)
			}
		}
	}

	serviceDocs := []string{
		filepath.Join(root, "dist", "bucks.service"),
		filepath.Join(root, "dist", "bucks-service-windows.md"),
	}
	for _, path := range serviceDocs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lower := strings.ToLower(string(data))
		if !strings.Contains(lower, "first chat") || !strings.Contains(lower, "paired") {
			t.Errorf("%s must document first-message Telegram pairing as the default", filepath.Base(path))
		}
		if !strings.Contains(lower, "optional") || !strings.Contains(lower, "lock") {
			t.Errorf("%s must keep BUCKS_TELEGRAM_CHAT_ID documented as an optional pre-lock override", filepath.Base(path))
		}
	}
}

func TestWindowsServiceDocsDoNotPastePlaceholderChatOverride(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "dist", "bucks-service-windows.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Windows service doc: %v", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		hasPlaceholder := strings.Contains(line, "BUCKS_TELEGRAM_CHAT_ID=000000000") ||
			strings.Contains(line, `BUCKS_TELEGRAM_CHAT_ID "000000000"`)
		if hasPlaceholder && !strings.HasPrefix(trimmed, "#") {
			t.Errorf("placeholder chat override must be commented so default first-message pairing stays default: %q", line)
		}
	}
}

func TestWindowsServiceDocsConfirmPairThenCommand(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "dist", "bucks-service-windows.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Windows service doc: %v", err)
	}
	lower := strings.ToLower(string(data))

	if !strings.Contains(lower, "message once to pair") {
		t.Fatalf("Windows service doc must tell the operator the first message pairs the chat")
	}
	if !strings.Contains(lower, "then send") || !strings.Contains(lower, "/status") {
		t.Fatalf("Windows service doc must tell the operator to send /status after pairing")
	}
	if !strings.Contains(lower, "does not run a command") {
		t.Fatalf("Windows service doc must explain that the first pairing message does not run a command")
	}
}
