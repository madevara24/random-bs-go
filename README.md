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
  `HTTP_PORT` (default `8420`), `DEFAULT_ENGINE` (default `claude` — the
  coding engine a task resolves to when neither its note's `engine:` nor
  its repo's `"engine"` is set), `CLAUDE_BIN` (default `claude` — the
  claude CLI's path/name, resolved via `PATH`), `REVIEW_ENGINE` (default
  `claude` — the engine that reviews a PR in the merge gate when neither
  the note's `review_engine:` nor the repo's `"review_engine"` is set,
  resolved independently of `DEFAULT_ENGINE`). `DEFAULT_ENGINE`,
  `REVIEW_ENGINE`, and every repo's `"engine"`/`"review_engine"` must each
  name a registered engine (today, only `claude`) — an unregistered name
  fails startup with a clear error.
- **`repos.json`** — a map of repo key to onboarding config:

  ```json
  {
    "my-repo": {
      "path": "/path/to/local/clone",
      "remote": "git@github.com:org/my-repo.git",
      "default_branch": "main",
      "ci": "...",
      "auto_merge_default": false,
      "engine": "claude",
      "review_engine": "claude"
    }
  }
  ```

  `path`, `remote`, and `default_branch` are required per repo. `engine`
  and `review_engine` are optional per-repo overrides of `DEFAULT_ENGINE`/
  `REVIEW_ENGINE` (see below).

### Engine selection

Each task can pin which engine codes it and which engine reviews its PR in
the merge gate, via the task note's own frontmatter:

```yaml
engine: claude
review_engine: claude
```

Both are optional; a note without either key resolves to `claude` today
(the only engine that exists). Resolution order, highest precedence
first:

- **Coding engine**: the note's `engine:` → the repo's `"engine"` →
  `DEFAULT_ENGINE`.
- **Review engine**: the note's `review_engine:` → the repo's
  `"review_engine"` → `REVIEW_ENGINE`.

The coding engine is resolved once, when the task is first claimed, and
written into the note's `engine:` the moment the run actually starts (not
earlier — until then, an `engine:` key on the note always means the task
author pinned it). A resume always reuses the note's existing `engine:`,
since one engine's `session_id` means nothing to another. The review
engine is resolved fresh each time the merge gate starts and is never
written back to the note (the reviewer has no session to resume).

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
