package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// telegram_trust.go persists the operator's trusted Telegram chat id next to the config, so a
// wizard-only owner who paired once (by messaging the bot) never has to set an env var. The
// bot token is a secret only the owner holds, so the first chat to message is the owner; and no
// Telegram command can move money (only /halt /resume /status /summary /positions), which
// bounds the blast radius of a mis-pair.

// telegramTrustPath is the trust file next to the config (e.g. .../bucks/telegram_trust.json).
func telegramTrustPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "telegram_trust.json")
}

type telegramTrust struct {
	ChatID int64 `json:"chat_id"`
}

// loadTrustedChatID resolves the operator's trusted Telegram chat id: BUCKS_TELEGRAM_CHAT_ID
// wins (explicit operator config), else a previously-PAIRED id persisted next to the config,
// else 0 (unpaired — the gateway then enters opt-in first-message pairing).
func loadTrustedChatID(configPath string) int64 {
	if id := trustedChatIDFromEnv(); id != 0 {
		return id
	}
	data, err := os.ReadFile(telegramTrustPath(configPath))
	if err != nil {
		return 0
	}
	var t telegramTrust
	if json.Unmarshal(data, &t) != nil {
		return 0
	}
	return t.ChatID
}

// saveTrustedChatID persists a paired operator chat id (0600) so it survives restarts. A zero
// id is refused — persisting 0 would later be read as "trust everyone".
func saveTrustedChatID(configPath string, id int64) error {
	if id == 0 {
		return fmt.Errorf("refusing to persist chat id 0 (would trust everyone)")
	}
	data, err := json.Marshal(telegramTrust{ChatID: id})
	if err != nil {
		return err
	}
	path := telegramTrustPath(configPath)
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}
