package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// keyringService is the service name BUCKS registers its secret under in the OS
// keychain. The user account namespaces multiple installs on one machine.
const keyringService = "bucks-trader"

// defaultKeyringUser is the keychain "account" the config is stored under when the
// caller does not specify one.
const defaultKeyringUser = "config"

// KeyringStore persists the Config in the OS keychain (Windows Credential Manager /
// macOS Keychain / Linux Secret Service) via go-keyring. The OS guards the secret with
// the user's login; BUCKS stores the YAML config as the secret value. The keychain is
// itself the encryption-at-rest, so no passphrase is needed here.
//
// This backend is unavailable on a headless Linux box with no Secret Service (no
// D-Bus) and in CI — Open() detects that and falls back to the FileStore. The keychain
// path is exercised in an integration test ONLY when a service is actually present
// (probed, never assumed), so the default suite stays deterministic and offline.
type KeyringStore struct {
	Service string
	User    string
}

// NewKeyringStore builds a KeyringStore under BUCKS's service and the given user
// account (empty -> the default account).
func NewKeyringStore(user string) *KeyringStore {
	if user == "" {
		user = defaultKeyringUser
	}
	return &KeyringStore{Service: keyringService, User: user}
}

// Backend names this backend.
func (s *KeyringStore) Backend() string { return "keyring" }

// Save stores the marshaled Config as the keychain secret value. The OS keychain
// encrypts it at rest; the plaintext exists only in memory en route to the keychain.
func (s *KeyringStore) Save(cfg Config) error {
	b, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := keyring.Set(s.Service, s.User, string(b)); err != nil {
		return fmt.Errorf("secrets: keyring set: %w", err)
	}
	return nil
}

// Load retrieves and parses the Config from the keychain. A missing entry maps to
// ErrNotFound; any other keychain error is surfaced.
func (s *KeyringStore) Load() (Config, error) {
	v, err := keyring.Get(s.Service, s.User)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return Config{}, ErrNotFound
		}
		return Config{}, fmt.Errorf("secrets: keyring get: %w", err)
	}
	return unmarshalConfig([]byte(v))
}

// Delete removes BUCKS's keychain entry (used by tests to leave no residue, and
// available to the runtime for a clean uninstall). A missing entry is not an error.
func (s *KeyringStore) Delete() error {
	if err := keyring.Delete(s.Service, s.User); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("secrets: keyring delete: %w", err)
	}
	return nil
}

// available reports whether the OS keychain is actually usable on this box right now.
// It does a tiny round-trip probe (set+get+delete of a throwaway value) so we only
// claim the keychain when it genuinely works — a headless Linux with no Secret Service
// fails the probe and the caller falls back to the file backend. This is a real probe,
// not an assumption (the verify-capability-first law).
func (s *KeyringStore) available() bool {
	const probeUser = "__bucks_probe__"
	const probeVal = "ok"
	if err := keyring.Set(s.Service, probeUser, probeVal); err != nil {
		return false
	}
	got, err := keyring.Get(s.Service, probeUser)
	_ = keyring.Delete(s.Service, probeUser) // best-effort cleanup
	return err == nil && got == probeVal
}

// compile-time assertion KeyringStore is a Store.
var _ Store = (*KeyringStore)(nil)

// openOptions tunes backend selection. The zero value is the PRODUCTION default
// (prefer the OS keychain, fall back to the age file). Tests use ForceFile to pin the
// hermetic file backend so they never touch the developer's real OS keychain — a
// process-wide, machine-wide global resource that two test binaries would otherwise
// collide on. This option does NOT change any production call site: production calls
// Open with no options and keeps preferring the keychain.
type openOptions struct {
	forceFile  bool
	workFactor int // hermetic-test scrypt cost (0 = production strong default)
}

// hermeticTestWorkFactor is the LOW scrypt cost used by the ForceFileBackend test seam
// so hermetic round-trip tests stay fast (production uses the strong scryptWorkFactor).
// scrypt at the production factor (18, ~1s) is ~1000x slower than this; under the race
// detector on a slow CI runner that turned a hermetic test's KDF into a multi-second
// stall that blew a test deadline. ForceFileBackend is TEST-ONLY, so a cheap KDF here is
// correct — it never touches a real on-disk secret (production prefers the keychain or
// NewFileStore's strong default).
const hermeticTestWorkFactor = 8

// Option configures Open. The exported options are deliberately test-isolation seams;
// production code passes none and gets the keychain-preferred behavior unchanged.
type Option func(*openOptions)

// ForceFileBackend makes Open skip the OS-keychain probe and use ONLY the age-encrypted
// file backend at filePath. It exists so tests are HERMETIC — every secret goes to a
// t.TempDir() file with a test passphrase, never the real machine keychain (which is a
// shared global the OS guards and which separate test binaries clobber). Production never
// passes this; production keeps preferring the keychain.
func ForceFileBackend() Option {
	return func(o *openOptions) {
		o.forceFile = true
		o.workFactor = hermeticTestWorkFactor // keep hermetic tests fast (test-only seam)
	}
}

// Open selects the best available backend: the OS keychain when it actually works,
// otherwise the age-encrypted file backend at filePath locked by passphrase. The
// fallback is automatic so a headless install still protects secrets — but the file
// backend needs a passphrase, so when the keychain is unavailable AND no passphrase is
// supplied, Open returns an error rather than silently writing weaker secrets.
//
// user namespaces the keychain entry (empty -> default). filePath/passphrase are the
// file-backend fallback parameters. opts is empty in production (keychain preferred); the
// ForceFileBackend test seam pins the file backend so tests stay hermetic.
func Open(user, filePath, passphrase string, opts ...Option) (Store, error) {
	var o openOptions
	for _, fn := range opts {
		fn(&o)
	}
	if o.forceFile {
		// Hermetic test path: no keychain probe, no global state — file backend only,
		// with a cheap scrypt cost so tests stay fast (workFactor is set by
		// ForceFileBackend; a 0 guard falls back to the strong default just in case).
		if o.workFactor > 0 {
			return newFileStoreWithWorkFactor(filePath, passphrase, o.workFactor)
		}
		return NewFileStore(filePath, passphrase)
	}
	ks := NewKeyringStore(user)
	if ks.available() {
		return ks, nil
	}
	if passphrase == "" {
		// Wrap the sentinel so cmd/bucks can errors.Is this exact "no keychain, no
		// passphrase" case and prompt for one, while the message stays descriptive.
		return nil, fmt.Errorf("OS keychain unavailable and no passphrase given for the encrypted-file fallback: %w", ErrPassphraseRequired)
	}
	return NewFileStore(filePath, passphrase)
}
