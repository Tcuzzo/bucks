<p align="center">
  <img src="assets/bucks-logo.png" alt="BUCKS — the 8-point buck" width="340">
</p>

# BUCKS

**Version v1.2.2** · MIT · paper-trading by default · check yours with `bucks version`

**BUCKS is a trading agent — a predator, not an assistant.** The name is a play on
*buck* (the deer) and *bucks* (money): an **8-point buck** with a dollar-sign motif who
works the markets for you inside the guardrails you set. It is not a chatbot you ask
questions; it is a trader you point at the market and let run — carefully, on a leash you
control.

It talks **technical** to a pro (RSI, ATR, position sizing, slippage) and **plain** to a
first-timer, switching by who it is talking to.

> **Honest by design.** BUCKS optimizes for **capability and risk control** — sticking to
> your plan, sizing trades safely, and stopping when it should. **It does not promise
> profit.** Markets carry real risk; no trading software can guarantee gains, and BUCKS
> will never pretend otherwise. There are no fake backtests and no invented "edge."

---

## What you get

- **A working trader on day one** — proven baseline strategies (momentum, mean-reversion,
  breakout) plus a playbook-driven analyst, all behind a risk engine.
- **Paper trading on by default.** BUCKS starts in **simulation** with fake money. It only
  trades real money after you **explicitly** flip it to live — there is no accidental
  live trading.
- **Hybrid autonomy (you stay in control).** Inside a per-trade **size/risk band** you
  set, BUCKS places trades on its own. Anything **bigger than the band** pauses and **asks
  you to Approve in Telegram** — and waits. If you deny it, or you don't answer, it does
  **not** place the trade. Fail-safe, always.
- **Circuit breakers.** A durable kill switch halts trading on a drawdown breach or daily
  loss limit — and stays halted across a restart until you clear it.
- **Your secrets stay protected.** Your broker keys, Telegram token, and AI keys are
  **encrypted at rest** — in your operating system's keychain when available, or in an
  encrypted file (locked by a passphrase) on a headless server. **Never stored in plain
  text, never in an environment variable, never committed to a repo.**

---

## Talk to BUCKS (v1.1)

BUCKS isn't just a config screen — you can **talk to him like a person**:

- **Chat** (`bucks chat`) — ask him anything, plain or technical; he code-switches to match you, stays honest, and **won't invent your account numbers or promise profit**. Any figure he states about your account is grounded against the real numbers.
- **Summaries** (`bucks summary`) — a plain-English "here's where you stand" of your P&L, positions, and health, with the numbers checked against reality.
- **Research** (`bucks research "<topic>"`, `bucks read <url>`) — read-only web lookups for market context, every claim traceable to a cited source. No stray orders, no headless browser — it stays one clean binary.
- **A free brain** — no Ollama and no paid key? Pick **Free (NVIDIA Nemotron)** at setup, paste a free `nvapi-` key from build.nvidia.com (~2 min, no card), and you're running. Groq / Cerebras / OpenRouter work the same way.

## Getting started — the guided unwrap

BUCKS ships as a **single file** (one static binary, no installer to fight). The guided
setup walks you through everything on first run: connecting your broker, your Telegram
bot, your AI backend, and writing your **playbook** (how much to risk, your style, your
goals). It runs the **same way on Linux and Windows**.

### Install (one command)

The fastest way in — installs BUCKS if it's missing, **updates** it if it's already
there, and verifies the download against the published checksums before touching your
machine. No download-and-unzip, no admin, no reinstall churn.

**macOS / Linux**
```sh
curl -fsSL https://raw.githubusercontent.com/Tcuzzo/bucks/main/install.sh | bash
```

**Windows (PowerShell)**
```powershell
irm https://raw.githubusercontent.com/Tcuzzo/bucks/main/install.ps1 | iex
```

The binary lands in a user-local folder (`~/.local/bin` on macOS/Linux,
`%LOCALAPPDATA%\BUCKS` on Windows). To **update later**, just re-run the same command —
or run `bucks update`. Then start it with `bucks` (the installer prints how, and the
exact `PATH` line to add if needed). It runs the **same way on Linux and Windows**.

### Download a zip instead (manual)

Prefer to grab it by hand? Download the zip for your computer from the
**[latest release](https://github.com/Tcuzzo/bucks/releases/latest)** (Windows, macOS, or
Linux) — no Go or build tools needed. Or build from source: `git clone` this repo, then
`go build -o bucks ./cmd/bucks`.

#### Linux / macOS
```sh
unzip BUCKS_linux_amd64.zip
cd BUCKS_linux_amd64
./install.sh          # guided unpack - walks you through first-run setup
```

#### Windows
```powershell
# Windows blocks downloaded scripts by default. Allow it for THIS session only:
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force
Expand-Archive BUCKS_windows_amd64.zip
cd BUCKS_windows_amd64
.\install.ps1          # guided unpack - walks you through first-run setup
```

> **Windows PowerShell 5.1 note.** Run each line on its own. Older PowerShell does
> **not** support `&&` to chain commands (`git clone ... && cd ...`) the way Linux/macOS
> shells do — paste the lines one at a time, or use `;` between them. The one-command
> `irm ... | iex` installer above sidesteps both the execution-policy prompt and `&&`
> entirely, so it's the easiest path on Windows.

On first run you'll see the wizard. Answer the questions, and BUCKS connects to your
broker's **paper** account and reaches **"trading (paper)"** — placing and managing
simulated trades inside your band. When you're satisfied, you can review going live.

After setup, just run `bucks` (or `bucks.exe`) to open the live dashboard, or run it
headless under a service manager with `bucks --daemon`.

---

## Going live (deliberately)

Live trading is a **deliberate flip**, never a default. You connect **live** broker keys
and explicitly enable live mode during setup. Even then, BUCKS only auto-trades **within
your band** and asks you to approve anything above it. You can halt everything at any time.

---

## Safety summary (plain English)

- **Paper first.** Simulated money until you choose otherwise.
- **You approve the big ones.** Above-band trades wait for your Telegram "Approve."
- **It stops itself.** Drawdown / daily-loss breakers halt trading and survive restarts.
- **Your keys are encrypted.** Keychain or passphrase-encrypted file — never plaintext.
- **No promises of profit.** BUCKS controls risk and follows your plan; the market does
  the rest.

---

## Under the hood

For the curious, BUCKS is built like a piece of trading infrastructure, not a script:

- **Crash-safe by design.** Orders are written to a journal and `fsync`'d *before* they're
  sent, with deterministic idempotency keys, so a crash mid-trade never loses or duplicates an
  order. On startup BUCKS reconciles against the broker's truth before it arms.
- **One engine, two clocks.** The same deterministic event engine runs both backtests and live
  trading, proven by a bit-for-bit replay test — so what you test is what you trade.
- **Exact money math.** Prices, sizes, and P&L use fixed-point decimals end to end. No float
  rounding ever touches your money.
- **One static file.** Pure Go, no C dependencies — a single binary that cross-compiles to
  Linux, Windows, and macOS, including an embedded pure-Go database.
- **Honest AI.** The optional LLM analyst is grounded against real evidence; unsupported claims
  are flagged, never presented as fact. No fabricated edge.

Go ~1.26. Built test-first; the suite covers the engine, the safety layer, and the unwrap path,
including a strict race-condition pass.

## License & credits

BUCKS is **MIT licensed** (see `LICENSE`). It links several excellent open-source Go
libraries, all under permissive licenses — credited in `NOTICE`. A build-time license
gate **hard-fails on any copyleft (A)GPL/LGPL dependency**, so BUCKS stays cleanly MIT.

The trading-engine patterns BUCKS uses (a deterministic event kernel, an order durability
spine, a capability probe) were **studied from the best prior art and re-implemented as
BUCKS's own** — inspired by, not copied. Details in `NOTICE`.
