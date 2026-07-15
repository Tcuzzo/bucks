# Run BUCKS 24/7 on Windows (always-on Telegram gateway)

This runs `bucks --daemon` in the background whenever you log in, so you can reach your
trader on Telegram (`/status`, `/halt`, `/resume`, `/summary`, `/positions`, `/help`)
without keeping a window open. Two ways — Task Scheduler (built in) or NSSM (a real
background service). Either is fine; pick one.

## The headless passphrase (read this first)

On a machine with no secure keychain, BUCKS unlocks your encrypted secrets (broker keys,
Telegram token) with a **passphrase**. Set it as an environment variable so the daemon can
start unattended:

```powershell
# Set it for your user account (persists across logins):
setx BUCKS_PASSPHRASE "your-passphrase-here"
```

Without `BUCKS_PASSPHRASE`, the daemon prints a clear message and exits instead of hanging.
You do **not** need to set `BUCKS_TELEGRAM_CHAT_ID`: the first chat to message your BUCKS
bot is paired automatically and remembered. To lock the operator chat before first contact,
set it explicitly:

```powershell
# setx BUCKS_TELEGRAM_CHAT_ID "replace-with-your-chat-id"
```

## Option A — Task Scheduler (run at logon)

1. Open **Task Scheduler** → **Create Task** (not "Basic Task").
2. **General** tab: name it `BUCKS`; check **Run only when user is logged on**.
3. **Triggers** tab → **New** → Begin the task: **At log on** → your user.
4. **Actions** tab → **New** → Program/script: the full path to `bucks.exe`;
   Add arguments: `--daemon`.
5. **Settings** tab: check **If the task fails, restart every** 1 minute, up to 3 times
   (so it self-heals).
6. OK. It starts at your next logon; **Run** it now to start immediately.

Task Scheduler inherits the `setx` environment variables above.

## Option B — NSSM (a true background service)

[NSSM](https://nssm.cc) runs `bucks --daemon` as a Windows **service** that starts at boot
and restarts on crash — no logon required.

```powershell
# Install the service (point at your bucks.exe):
nssm install BUCKS "C:\Path\To\bucks.exe" --daemon

# Give the service the passphrase (services don't see your setx vars):
nssm set BUCKS AppEnvironmentExtra BUCKS_PASSPHRASE=your-passphrase-here

# Optional: lock the operator chat before first contact instead of first-message pairing.
# nssm set BUCKS AppEnvironmentExtra BUCKS_PASSPHRASE=your-passphrase-here BUCKS_TELEGRAM_CHAT_ID=replace-with-your-chat-id

# Auto-restart on exit, and start it:
nssm set BUCKS AppExit Default Restart
nssm start BUCKS
```

Manage it later with `nssm restart BUCKS`, `nssm stop BUCKS`, or `nssm remove BUCKS`.

## Confirm it works

Open Telegram and message once to pair this chat with your BUCKS bot; the first message
confirms pairing and does not run a command. Then send **`/status`** — it should reply with
your trading mode, broker, equity, and halt state.
