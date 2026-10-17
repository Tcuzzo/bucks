# Dependency maintenance

CI and releases use the newest Go 1.26 patch, while go.mod records the minimum
patched toolchain (1.26.9). When upgrading the Go minor release, update all three
workflows and go.mod together. The native vulnerability scan fails on reachable
vulnerabilities, including the standard library; do not suppress it to merge an
unrelated dependency update.

Dependabot opens weekly grouped minor/patch updates for Go modules and Actions.
Major updates remain separate. Automatic rebasing reduces stale-branch conflicts;
it does not replace reviewing compatibility or waiting for passing checks.

The daily Dependency health workflow scans even when no source code changes.
It needs no CRDA or Snyk credentials. CI and release builds also verify downloaded
module checksums and fail when go.mod/go.sum need tidying.

Before merging a dependency update, run:

```sh
go mod download
go mod verify
go mod tidy -diff
go build ./...
go vet ./...
go test ./... -count=1
go test ./... -race -count=1
govulncheck ./...
```

Resolve go.sum conflicts by merging the intended go.mod changes and regenerating
checksums with go mod tidy; never pick one side blindly. Retain the license gate
and cross-platform build checks. Required status checks in GitHub branch rules
should include CI and Dependency health; workflow files alone cannot enforce
branch protection. No automated dependency merge is enabled by these files.
