# CLAUDE.md

Guidance for working in this repo.

## Go toolchain

`go` may not be on `PATH` in every environment this repo gets checked out
into. Run `which go` first; if it's missing, search for a `go` binary under
a toolchain install directory (e.g. `find / -maxdepth 4 -iname go -type f`)
and add it to `PATH` for the session before running `go build`/`go
test`/`go vet`.

## Running tests

`go test ./...` passing green is not sufficient on its own. Most of
`internal/runner` and `internal/watcher`'s integration tests depend on a
throwaway PM-vault-shaped git fixture on disk — see `internal/testvault`'s
`Path` constant for where it looks (it can differ per checkout, so don't
assume a specific path here). If that fixture is missing, those tests skip
silently rather than fail.

Always run with `-v` and check for `--- SKIP` lines before trusting a test
run as real coverage:

```
go test ./... -v 2>&1 | grep -- '--- SKIP'
```

`internal/notify` has a test that fires a real Discord webhook message.
It's gated behind `PMRUNNER_SEND_REAL_DISCORD_TEST=1` and skips by default
— do not set that env var unless you specifically intend to send a real
message.

## runner package layout

`internal/runner/crashfallback.go` and `internal/runner/setupfallback.go`
are separate files for a reason and shouldn't be merged:

- `setupfallback.go` handles failures in the five steps that run *before*
  Claude is ever invoked (claim write, re-read, branch setup, note-copy
  write, git-exclude).
- `crashfallback.go` handles the three scenarios where Claude *did* run
  but didn't finish cleanly (no result copy, an unparseable copy, or a
  copy that parsed but never reached a terminal status).

Both write `status: blocked` to the vault note and fire the same alert
path, but the failure information available at each stage is different,
which is why they're kept as distinct code paths.
