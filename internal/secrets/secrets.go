// Package secrets is BUCKS's secrets-at-rest layer. The guided-unpack wizard collects
// the owner's broker key+secret, Telegram bot token, and LLM API key(s) — the exact
// material that, if leaked, drains a trading account. The operator's law (spec §9) is
// blunt: NEVER plaintext, NEVER env vars, NEVER committed. This package is how that
// config reaches disk: always encrypted, behind a typed Store.
//
// TWO BACKENDS behind one Store interface (pure-Go, CGO_ENABLED=0 — no cgo):
//
//   - KeyringStore — the OS keychain (Windows Credential Manager / macOS Keychain /
//     Linux Secret Service over D-Bus) via go-keyring. PREFERRED on a desktop: the OS
//     guards the secret with the user's login. It needs a running secret service, so
//     it is unavailable on a headless Linux box (no D-Bus) and in CI.
//   - FileStore — an age-encrypted file (age scrypt / passphrase recipient) for the
//     headless case. The owner supplies a passphrase; the config is age-encrypted to a
//     0600 file. This is the DETERMINISTIC, always-available backend (and what the
//     tests exercise). It is real authenticated encryption (ChaCha20-Poly1305 under
//     age's STREAM), not obfuscation.
//
// Open() picks the keychain when it actually works and falls back to the file backend
// otherwise — the fallback is automatic so a headless install still protects secrets.
//
// The Config persisted here is BUCKS's own serializable shape (broker creds, tokens,
// keys) — secrets does NOT import the TUI, so there is no import cycle and the layer is
// reusable. cmd/bucks maps a tui.SetupResult into this Config before saving.
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

// ErrNotFound is returned by Load when no config has been saved yet (first run).
var ErrNotFound = errors.New("secrets: no saved config found")

// ErrPassphraseRequired is the typed, errors.Is-able sentinel returned whenever a
// passphrase is needed to protect the keys at rest but none was supplied. It surfaces
// from BOTH no-passphrase failure paths: opening the encrypted-FILE backend with an
// empty passphrase, and Open falling back to that file backend on a box with no usable
// OS keychain and no passphrase given. cmd/bucks branches on this sentinel (errors.Is)
// to securely PROMPT the owner for a passphrase instead of dying with a cryptic error —
// the keychain-less first-run fix.
var ErrPassphraseRequired = errors.New("secrets: a passphrase is required to encrypt your keys on this machine (no system keychain available)")

// BrokerCred is one broker connection's credentials as persisted (kind + key/secret).
// Kind mirrors the wizard's broker selection (e.g. "alpaca-paper"); Key/Secret are the
// real venue credentials.
type BrokerCred struct {
	Kind   string `yaml:"kind"`
	Key    string `yaml:"key"`
	Secret string `yaml:"secret"`
}

// Config is the secret-bearing configuration persisted at rest. It is the serializable
// subset of the wizard's result that MUST be encrypted: broker creds, the Telegram bot
// token, and the LLM key(s). Non-secret runtime knobs (the playbook) live in the plain
// config file; this struct is ONLY the sensitive material.
type Config struct {
	TelegramToken string       `yaml:"telegram_token"`
	LLMChoice     string       `yaml:"llm_choice"`
	LLMKeys       []string     `yaml:"llm_keys,omitempty"`
	Brokers       []BrokerCred `yaml:"brokers"`
	// Live is the owner's persisted live-trading ARM. It rides with the encrypted creds
	// (not the plain config) because it is operationally sensitive — it is the recorded
	// intent that the owner configured a live broker and chose to go live. Persisting it
	// only means BUCKS REMEMBERS the arm across restarts; actually placing live orders
	// still requires a deliberate per-session confirmation in the trade loop (paper is the
	// boot default by construction). An older config without this field decrypts to false
	// (paper) — the safe default — so the addition is backward compatible.
	Live bool `yaml:"live"`
	// Voice is the owner's voice-surface preference (non-secret, carried here so the full
	// setup round-trips faithfully rather than being silently lost on save).
	Voice bool `yaml:"voice"`
}

// Marshal renders the Config to YAML bytes (the plaintext that the backends encrypt).
func (c Config) Marshal() ([]byte, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("secrets: marshal config: %w", err)
	}
	return b, nil
}

// unmarshalConfig parses YAML bytes back into a Config.
func unmarshalConfig(b []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("secrets: unmarshal config: %w", err)
	}
	return c, nil
}

// Store is the secrets-at-rest contract: persist and retrieve the Config, always
// encrypted. Save MUST NOT leave plaintext anywhere; Load returns ErrNotFound when
// nothing was saved.
type Store interface {
	Save(cfg Config) error
	Load() (Config, error)
	// Backend names the concrete backend ("keyring" | "file") for logs/telemetry.
	Backend() string
}

// --- FileStore: age-encrypted file backend (headless / CI / fallback) ---

// scryptWorkFactor is age's scrypt cost (log2 N). age's default is 18 (~1s); BUCKS
// uses a deliberately higher-but-fast-enough default for real installs. Tests inject
// a low factor via newFileStoreWithWorkFactor to stay quick without weakening the
// production path.
const scryptWorkFactor = 18

// FileStore encrypts the Config to an age scrypt (passphrase) file at Path. The file
// is written 0600 (owner read/write only). The passphrase is held only in memory for
// the duration of a Save/Load — it is never persisted.
type FileStore struct {
	Path       string
	passphrase string
	workFactor int
}

// NewFileStore builds a FileStore for an age-encrypted config at path, locked by
// passphrase. An empty passphrase is rejected — encrypting to an empty passphrase
// would be no protection at all.
func NewFileStore(path, passphrase string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("secrets: file store path is empty")
	}
	if passphrase == "" {
		// Wrap the sentinel so callers can branch with errors.Is while the message stays
		// human-readable (and pins which backend asked for it).
		return nil, fmt.Errorf("encrypted-file backend: %w", ErrPassphraseRequired)
	}
	return &FileStore{Path: path, passphrase: passphrase, workFactor: scryptWorkFactor}, nil
}

// newFileStoreWithWorkFactor is the test seam: same as NewFileStore but with an
// explicit (low) scrypt work factor so round-trip tests stay fast. NOT exported — the
// production path always uses the strong default.
func newFileStoreWithWorkFactor(path, passphrase string, logN int) (*FileStore, error) {
	fs, err := NewFileStore(path, passphrase)
	if err != nil {
		return nil, err
	}
	fs.workFactor = logN
	return fs, nil
}

// Backend names this backend.
func (s *FileStore) Backend() string { return "file" }

// Save age-encrypts the Config and writes it to Path with 0600 perms. It writes to a
// temp file in the same directory then renames, so a crash mid-write never leaves a
// half-written (corrupt) secrets file. The plaintext exists only in memory.
func (s *FileStore) Save(cfg Config) error {
	plain, err := cfg.Marshal()
	if err != nil {
		return err
	}
	enc, err := s.encrypt(plain)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("secrets: mkdir %q: %w", dir, err)
		}
	}
	// Atomic write: temp file (0600) then rename onto the target.
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".bucks-secrets-*.age")
	if err != nil {
		return fmt.Errorf("secrets: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: chmod temp: %w", err)
	}
	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: write encrypted: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("secrets: rename into place: %w", err)
	}
	// Belt-and-suspenders: enforce 0600 on the final file too.
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return fmt.Errorf("secrets: chmod %q: %w", s.Path, err)
	}
	return nil
}

// Load reads and age-decrypts the Config from Path. A missing file is ErrNotFound; a
// wrong passphrase (or tampered file) surfaces a decrypt error (never a partial/garbage
// Config). The plaintext exists only in memory.
func (s *FileStore) Load() (Config, error) {
	enc, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, ErrNotFound
		}
		return Config{}, fmt.Errorf("secrets: read %q: %w", s.Path, err)
	}
	plain, err := s.decrypt(enc)
	if err != nil {
		return Config{}, err
	}
	return unmarshalConfig(plain)
}

// encrypt age-encrypts plain to a scrypt (passphrase) recipient.
func (s *FileStore) encrypt(plain []byte) ([]byte, error) {
	rcpt, err := age.NewScryptRecipient(s.passphrase)
	if err != nil {
		return nil, fmt.Errorf("secrets: scrypt recipient: %w", err)
	}
	rcpt.SetWorkFactor(s.workFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rcpt)
	if err != nil {
		return nil, fmt.Errorf("secrets: age encrypt: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("secrets: age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secrets: age close: %w", err)
	}
	return buf.Bytes(), nil
}

// decrypt age-decrypts enc with the scrypt (passphrase) identity. A wrong passphrase
// or tampered ciphertext returns an error — it never yields plaintext.
func (s *FileStore) decrypt(enc []byte) ([]byte, error) {
	id, err := age.NewScryptIdentity(s.passphrase)
	if err != nil {
		return nil, fmt.Errorf("secrets: scrypt identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(enc), id)
	if err != nil {
		// Wrong passphrase / corrupt file lands here — fail closed.
		return nil, fmt.Errorf("secrets: age decrypt failed (wrong passphrase or tampered file): %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secrets: age read plaintext: %w", err)
	}
	return out, nil
}

// compile-time assertion FileStore is a Store.
var _ Store = (*FileStore)(nil)
