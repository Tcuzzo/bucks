package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/channel"
	"bucks/internal/harness"
	"bucks/internal/orders"
	"bucks/internal/playbook"
	"bucks/internal/risk"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// bootResult is what a paper-trade boot produced: the constructed Trader, the broker
// it placed against, and whether it reached "trading (paper)" (a paper order actually
// placed). It is returned so the acceptance test can assert the end-to-end wiring
// without driving the live terminal.
type bootResult struct {
	Trader        *harness.Trader
	Broker        *mock.MockBroker
	ReachedPaper  bool   // true once a within-band paper order was placed
	PlacedClOrdID string // the deterministic client-order-id of the placed paper order
	Status        string // plain-English boot status, e.g. "trading (paper)"
}

// SecretConfigFrom maps the wizard's validated SetupResult into the secret-bearing
// secrets.Config that is encrypted at rest. ONLY the sensitive material crosses (broker
// creds, Telegram token, LLM keys) — the playbook (non-secret runtime mandate) is
// persisted separately in the plain config. This is the single seam where the TUI shape
// becomes the persisted-secrets shape, so secrets stays TUI-free (no import cycle).
func SecretConfigFrom(r tui.SetupResult) secrets.Config {
	brokers := make([]secrets.BrokerCred, 0, len(r.Brokers))
	for _, b := range r.Brokers {
		brokers = append(brokers, secrets.BrokerCred{
			Kind:   string(b.Kind),
			Key:    b.Key,
			Secret: b.Secret,
		})
	}
	cfg := secrets.Config{
		TelegramToken: r.TelegramToken,
		LLMChoice:     string(r.LLM),
		Brokers:       brokers,
		// Carry the owner's deliberate live-arm and voice preference so the full setup
		// survives a restart. Dropping r.Live here was the bug that silently reverted an
		// armed owner back to paper. Going live still needs a per-session confirmation in
		// the trade loop — persisting the arm is REMEMBERING the intent, not auto-trading.
		Live:  r.Live,
		Voice: r.VoiceEnabled,
	}
	// The LLM API key is sensitive (it authenticates chat/analyst calls), so it crosses
	// here into the ENCRYPTED secrets — never the plain config. Persisted only when the
	// owner actually has a key (the OAuth path carries none). LLMKeys[0] is the single
	// chat/analyst key; the slice keeps room for a future primary+fallback pair.
	if r.LLMKey != "" {
		cfg.LLMKeys = []string{r.LLMKey}
	}
	return cfg
}

// bootPaperTrader wires a loaded SetupResult into a running (paper) Trader and proves
// the unwrap -> config -> trader path end-to-end by driving ONE in-band paper trade
// decision through the real harness.Trader. It uses the in-memory mock broker (the
// paper venue for this acceptance — live Alpaca-paper needs the operator's keys), the
// risk engine built FROM THE PLAYBOOK, a durable kill switch at killSwitchPath, and a
// mock channel (no live Telegram). Paper is the default by construction: LiveEnabled is
// false unless the wizard explicitly armed live (and even then this acceptance routes to
// the paper/mock broker — going truly live needs the operator's live keys + flip).
//
// "Reaches trading (paper)" means: the Trader placed a within-band paper order on the
// broker (OutcomeAutoPlaced). That is the honest acceptance bar for the shipped zip's
// first-run unwrap.
func bootPaperTrader(ctx context.Context, r tui.SetupResult, killSwitchPath string) (bootResult, error) {
	// 1. Risk engine FROM THE PLAYBOOK — the owner's mandate becomes enforceable limits.
	engine := risk.NewEngine(r.Playbook.ToRiskConfig())

	// 2. Durable kill switch (its own file so a halt survives a restart). On a fresh
	//    boot with no prior halt it comes up clear.
	ks, err := risk.Open(killSwitchPath)
	if err != nil {
		return bootResult{}, fmt.Errorf("boot: open kill switch: %w", err)
	}

	// 3. Paper broker (in-memory mock = the paper venue for this acceptance) seeded with
	//    a funded account and a quote so a real proposal can be sized + placed.
	broker := mock.New()
	equity := r.Playbook.Capital
	broker.SetAccount(brokers.Account{Equity: equity, Cash: equity, BuyingPower: equity})

	// 4. Channel: a mock (no live Telegram) — the boot acceptance is offline. Above-band
	//    trades would ask here; our acceptance trade is WITHIN the band so it auto-places.
	ch := channel.NewMockChannel()

	// 5. A real hybrid band sized from the playbook capital so the acceptance trade is
	//    comfortably WITHIN it (auto-placed, no approval needed). The band is a fraction
	//    of capital; the acceptance proposal is far smaller.
	band := paperAcceptanceBand(equity)

	// 6. Deterministic clock — no wall clock, so the boot is reproducible/testable.
	fixed := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC) // a weekday, market-open hours
	now := func() time.Time { return fixed }

	trader, err := harness.NewTrader(harness.TraderConfig{
		StrategyName: "boot-acceptance",
		Engine:       engine,
		Broker:       broker,
		KillSwitch:   ks,
		Channel:      ch,
		Band:         band,
		Market:       harness.Always24x7{}, // crypto-style always-open for the offline acceptance
		Now:          now,
		LiveEnabled:  false, // PAPER default — never live in the boot acceptance
	})
	if err != nil {
		return bootResult{}, fmt.Errorf("boot: construct trader: %w", err)
	}

	res := bootResult{Trader: trader, Broker: broker, Status: "configured (paper, not yet traded)"}

	// 7. Drive ONE within-band paper decision through the full hybrid-autonomy path.
	decision := paperAcceptanceDecision(equity)
	ps := risk.PortfolioState{
		Equity:            equity,
		Cash:              equity,
		Positions:         map[string]risk.HeldPosition{},
		RealizedPnLToday:  orders.ZeroDecimal,
		OpenPositionCount: 0,
	}
	rec, err := trader.Tick(ctx, decision, ps, equity)
	if err != nil {
		return res, fmt.Errorf("boot: paper tick: %w", err)
	}
	if !rec.Outcome.Placed() {
		return res, fmt.Errorf("boot: paper trade did not place (outcome=%s, info=%q)", rec.Outcome, rec.RiskInfo)
	}

	res.ReachedPaper = true
	res.PlacedClOrdID = rec.ClOrdID
	res.Status = "trading (paper)"
	return res, nil
}

// paperAcceptanceDecision is one realistic, within-band BUY proposal with a protective
// stop (the risk engine requires a stop). Sized tiny relative to capital so it clears
// every risk limit and the hybrid band — the honest "a paper trade was placed" proof.
func paperAcceptanceDecision(equity orders.Decimal) harness.TradeDecision {
	entry := orders.MustParseDecimal("100")
	stop := orders.MustParseDecimal("99") // 1 point of risk per share
	qty := orders.MustParseDecimal("1")   // 1 share => $1 risk, $100 notional — tiny vs capital
	return harness.TradeDecision{
		HasProposal: true,
		Proposal: risk.OrderProposal{
			Symbol:        "BUCKS",
			Side:          orders.SideBuy,
			Qty:           qty,
			EntryPx:       entry,
			StopPx:        stop,
			AccountEquity: equity,
		},
		Reason: "boot acceptance: prove unwrap->config->trader reaches trading (paper)",
		Seq:    1,
	}
}

// paperAcceptanceBand sizes the auto-band as a fraction of capital so the small
// acceptance trade is within it (auto-placed). A floor keeps the band sane on a tiny
// capital. The acceptance proposal ($1 risk / $100 notional) sits comfortably inside.
func paperAcceptanceBand(capital orders.Decimal) harness.HybridBandConfig {
	// Auto-band notional cap = 50% of capital (or at least $1000), risk cap = 5% of
	// capital (or at least $50). The acceptance trade is well under both.
	notionalCap := fractionOrFloor(capital, "0.5", "1000")
	riskCap := fractionOrFloor(capital, "0.05", "50")
	return harness.HybridBandConfig{
		MaxAutoNotional:   notionalCap,
		MaxAutoRiskAmount: riskCap,
	}
}

// fractionOrFloor returns max(capital*frac, floor) in exact decimal.
func fractionOrFloor(capital orders.Decimal, frac, floor string) orders.Decimal {
	f := orders.MustParseDecimal(frac)
	fl := orders.MustParseDecimal(floor)
	v, err := capital.Mul(f)
	if err != nil {
		return fl
	}
	if v.Cmp(fl) < 0 {
		return fl
	}
	return v
}

// --- persistence: the split between the PLAIN config and the ENCRYPTED secrets ---
//
// The owner's playbook (the non-secret runtime mandate) is written PLAIN to the config
// file. The sensitive material (broker creds, tokens, keys) is written ENCRYPTED via the
// secrets store, NEVER to the plain config. This is the persistence split the spec
// requires (no secrets in the plain config; no plaintext at rest).

// secretsPathFor derives the encrypted-secrets file path that sits next to the plain
// config (e.g. .../bucks/bucks.yaml -> .../bucks/secrets.age). Used for the file
// backend; the keychain backend ignores it.
func secretsPathFor(configPath string) string {
	if sameConfigPath(configPath, defaultConfigPath()) || filepath.Base(configPath) == "bucks.yaml" {
		return legacySecretsPathFor(configPath)
	}
	name := filepath.Base(configPath)
	if name == "" || name == "." {
		name = "bucks"
	}
	return filepath.Join(filepath.Dir(configPath), name+".secrets.age")
}

func legacySecretsPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "secrets.age")
}

func secretsUserFor(configPath string) string {
	if sameConfigPath(configPath, defaultConfigPath()) {
		return ""
	}
	normalized := normalizedConfigPath(configPath)
	sum := sha256.Sum256([]byte(normalized))
	return "config-" + hex.EncodeToString(sum[:8])
}

func sameConfigPath(left, right string) bool {
	return normalizedConfigPath(left) == normalizedConfigPath(right)
}

func normalizedConfigPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func openPrimarySecretsStore(configPath, passphrase string, secretOpts ...secrets.Option) (secrets.Store, error) {
	return secrets.Open(secretsUserFor(configPath), secretsPathFor(configPath), passphrase, secretOpts...)
}

// loadSecretsConfig reads the config-specific store. A non-default config created by
// an older Bucks release may still use the legacy shared store; read it only when the
// new namespace is absent and return the primary store so the next save migrates it.
func loadSecretsConfig(configPath, passphrase string, secretOpts ...secrets.Option) (secrets.Store, secrets.Config, error) {
	primary, err := openPrimarySecretsStore(configPath, passphrase, secretOpts...)
	if err != nil {
		return nil, secrets.Config{}, err
	}
	cfg, err := primary.Load()
	if err == nil || !errors.Is(err, secrets.ErrNotFound) || secretsUserFor(configPath) == "" {
		return primary, cfg, err
	}
	legacy, legacyErr := secrets.Open("", legacySecretsPathFor(configPath), passphrase, secretOpts...)
	if legacyErr != nil {
		return nil, secrets.Config{}, legacyErr
	}
	cfg, legacyErr = legacy.Load()
	return primary, cfg, legacyErr
}

// SaveSetup persists a wizard SetupResult: the playbook plain to configPath, and the
// secret material encrypted via the chosen secrets.Store. The store decides the backend
// (keychain when available, else the age file). passphrase is used only by the file
// backend (it may be empty when the keychain is in use).
//
// secretOpts is empty for production callers (the keychain is preferred exactly as
// before); tests pass secrets.ForceFileBackend() so the round trip is HERMETIC and never
// writes into the developer's real OS keychain.
func SaveSetup(r tui.SetupResult, configPath, passphrase string, secretOpts ...secrets.Option) error {
	if dir := filepath.Dir(configPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("save setup: mkdir %q: %w", dir, err)
		}
	}
	pbYAML, err := yaml.Marshal(r.Playbook)
	if err != nil {
		return fmt.Errorf("save setup: marshal playbook: %w", err)
	}
	// Open the secrets store FIRST — before writing the plain config — so a missing
	// passphrase (ErrPassphraseRequired on a keychain-less box) aborts the save BEFORE
	// any file lands on disk. Otherwise an orphan plain config would make configExists
	// report a completed setup that has no secrets, and the next launch would try the
	// dashboard and fail to load. (errors.Is still sees ErrPassphraseRequired through %w.)
	store, err := openPrimarySecretsStore(configPath, passphrase, secretOpts...)
	if err != nil {
		return fmt.Errorf("save setup: open secrets store: %w", err)
	}
	// Save the encrypted secrets FIRST, then write the plain config LAST. configExists()
	// keys off the plain config as the "setup completed" signal, so writing it only after
	// the secrets are durably stored means NO failure path (disk-full, EIO, cancel) can
	// leave an orphan config that claims a completed setup the next launch can't load.
	if err := store.Save(SecretConfigFrom(r)); err != nil {
		return fmt.Errorf("save setup: encrypt secrets: %w", err)
	}
	if err := os.WriteFile(configPath, pbYAML, 0o600); err != nil {
		return fmt.Errorf("save setup: write config: %w", err)
	}
	return nil
}

// LoadSetup reconstructs a SetupResult from the persisted plain config (playbook) +
// encrypted secrets. It is the inverse of SaveSetup and is what the runtime uses to boot
// from a prior unwrap. The reconstructed result is paper by default (Live is not carried
// in this slice's persistence — going live is a deliberate runtime flip).
//
// secretOpts mirrors SaveSetup: empty in production (keychain preferred), and
// secrets.ForceFileBackend() in tests so the load reads from the same hermetic file the
// hermetic save wrote — never the real OS keychain.
func LoadSetup(configPath, passphrase string, secretOpts ...secrets.Option) (tui.SetupResult, error) {
	pbYAML, err := os.ReadFile(configPath)
	if err != nil {
		return tui.SetupResult{}, fmt.Errorf("load setup: read config: %w", err)
	}
	var pb playbook.Playbook
	if err := yaml.Unmarshal(pbYAML, &pb); err != nil {
		return tui.SetupResult{}, fmt.Errorf("load setup: parse playbook: %w", err)
	}
	if err := pb.Validate(); err != nil {
		return tui.SetupResult{}, fmt.Errorf("load setup: playbook invalid: %w", err)
	}
	_, sc, err := loadSecretsConfig(configPath, passphrase, secretOpts...)
	if err != nil {
		return tui.SetupResult{}, fmt.Errorf("load setup: decrypt secrets: %w", err)
	}
	brokerCreds := make([]tui.BrokerCreds, 0, len(sc.Brokers))
	for _, b := range sc.Brokers {
		brokerCreds = append(brokerCreds, tui.BrokerCreds{
			Kind:   tui.BrokerKind(b.Kind),
			Key:    b.Key,
			Secret: b.Secret,
		})
	}
	llmKey := ""
	if len(sc.LLMKeys) > 0 {
		llmKey = sc.LLMKeys[0]
	}
	return tui.SetupResult{
		TelegramToken: sc.TelegramToken,
		LLM:           tui.LLMChoice(sc.LLMChoice),
		LLMKey:        llmKey,
		Brokers:       brokerCreds,
		Playbook:      pb,
		VoiceEnabled:  sc.Voice,
		// The persisted live-ARM is carried through faithfully (sc.Live) rather than forced
		// to false — an armed owner stays armed across restarts. This is NOT auto-live: the
		// trade loop still defaults to paper and requires a deliberate per-session
		// confirmation before any real order is placed. An old config (no live field)
		// decrypts to false, the safe paper default.
		Live: sc.Live,
	}, nil
}

// runPaperSmoke is the real CLI acceptance path (`bucks --paper-smoke`): load the saved
// setup (plain playbook + encrypted secrets), wire a paper trader, drive one in-band
// paper trade, and print whether it reached "trading (paper)". It reads the passphrase
// for the file backend from BUCKS_PASSPHRASE (so the keychain path needs no env). This
// runs the ACTUAL unwrap->config->trader chain end-to-end, offline. Live Alpaca-paper
// is the operator-gated next step (needs their keys).
func runPaperSmoke(configPath string) error {
	passphrase := os.Getenv("BUCKS_PASSPHRASE")
	r, err := LoadSetup(configPath, passphrase)
	if err != nil {
		return fmt.Errorf("paper-smoke: %w", err)
	}
	ksPath := filepath.Join(filepath.Dir(configPath), "killswitch.json")
	res, err := bootPaperTrader(context.Background(), r, ksPath)
	if err != nil {
		return fmt.Errorf("paper-smoke: %w", err)
	}
	if !res.ReachedPaper {
		return fmt.Errorf("paper-smoke: did not reach trading (paper): %s", res.Status)
	}
	fmt.Printf("bucks: %s — placed paper order %s on the paper broker\n", res.Status, res.PlacedClOrdID)
	return nil
}
