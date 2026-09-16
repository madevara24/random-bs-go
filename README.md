# pmrunner

`pmrunner` is a task-runner pipeline for driving Claude Code against a set of
onboarded repos, tracked as task notes in a PM vault (a git repo of Markdown
files). It replaces an earlier bash/git-hook pipeline with a single Go
binary that runs as one of two modes:

- **runner** — the long-lived daemon. Watches the vault for tasks with
  `status: ready` (or `blocker_resolved`), claims them, checks out a branch
  in the target repo, invokes `claude -p` against the task prompt, watches
  for an idle timeout, merges the result back into the vault note, and
  (for `auto_merge: true` tasks) runs a review/CI merge-gate loop before
  merging the PR.
- **watcher** (`pmwatch`) — a separate polling process that checks the
  runner daemon's health, flags stale/wedged tasks, and catches any
  terminal-status note that never got a Discord notification.

## Build

```
go build ./cmd/pmrunner
```

## Run

Both modes read the same config directory (see below).

```
./pmrunner runner  [--config-dir=<dir>]
./pmrunner watcher [--config-dir=<dir>]
```

The config directory defaults to `$PMRUNNER_CONFIG_DIR`, or the current
working directory if that's unset.

## Config

Each config directory needs two files:

- **`.env`** — plain `KEY=VALUE` lines (no quoting/escaping beyond matched
  surrounding quotes). Required keys: `DISCORD_WEBHOOK_URL`,
  `DISCORD_USER_ID`, `DISCORD_ARA_DEV_USER_ID`, `GLOBAL_SLOTS` (positive
  int — the shared concurrency cap across all repos), `IDLE_TIMEOUT_MINUTES`
  (positive int), `VAULT_PATH`, `VAULT_DEFAULT_BRANCH`. Optional:
  `HTTP_PORT` (default `8420`).
- **`repos.json`** — a map of repo key to onboarding config:

  ```json
  {
    "my-repo": {
      "path": "/path/to/local/clone",
      "remote": "git@github.com:org/my-repo.git",
      "default_branch": "main",
      "ci": "...",
      "auto_merge_default": false
    }
  }
  ```

  `path`, `remote`, and `default_branch` are required per repo.

## Test

```
go test ./...
```

Most of `internal/runner` and `internal/watcher`'s integration tests need a
throwaway PM-vault-shaped git fixture on disk (see `internal/testvault`'s
`Path` constant for where it looks). If that fixture is absent, those tests
skip silently rather than fail, so a green `go test ./...` on a fresh
checkout can mean "almost nothing ran," not "everything passed." Run with
`-v` and check for `--- SKIP` lines mentioning "throwaway test vault not
present" before trusting the result as real coverage.

`internal/notify`'s real-webhook test also skips by default; it only runs
with `PMRUNNER_SEND_REAL_DISCORD_TEST=1` set, since it fires a real Discord
message.
