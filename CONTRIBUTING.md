# Contributing to BUCKS

BUCKS is real-money software: paper by default, honest by design, one static binary.
Contributions are welcome under the same discipline.

## Build and test

```sh
go build ./...          # must build clean
go test ./... -count=1  # the full suite, no cached results
```

CI also runs exactly these — a PR that fails any of them will not merge:

```sh
gofmt -l .                    # must print nothing
go vet ./...
go test ./... -race -count=1  # the race detector is not optional in a trading loop
```

Everything runs offline. Tests never hit a real broker, a real LLM, or the network —
if your change needs a server, use `httptest` like the rest of the suite.

## Ground rules

- **Never weaken a safety rail.** The paper/live gate, the per-session live
  confirmation, the risk band, the kill switch, and the Telegram approval flow are
  the product. A test proving your change keeps them intact beats a paragraph
  explaining why it probably does.
- **Tests come with the change.** A fix ships with the test that fails without it.
- **Keep it one clean binary.** No new services, no daemons-on-the-side, no CGO
  unless there is truly no other way.
- **Secrets stay out of the repo.** `dist/secret_scan.sh .` runs in CI and will
  reject key-shaped strings; run it yourself before pushing.

## PR flow

1. Fork, branch from `main`.
2. Make the change with its tests; run the commands above until they're clean.
3. Open a PR describing what changed and why — plain language, like the README.
4. Review by the code owner (@Tcuzzo). Security-sensitive findings go through
   [SECURITY.md](SECURITY.md), never a public PR.
