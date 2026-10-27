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
  name a registered engine (`claude` or `pi`) — an unregistered name fails
  startup with a clear error.

  **`pi` engine config** (all optional, all about the `pi` engine
  specifically — see "Engine selection" below): `PI_BIN` (default `pi`,
  resolved via `PATH`), `PI_MODEL` (default empty — no `--model` is passed,
  so `pi` falls back to whatever model is configured in *its own* settings,
  see below), `PI_REVIEW_MODEL` (default: same as `PI_MODEL`, independent
  once set), `PI_SESSION_DIR` (default `pi-sessions` under the config
  directory; created once at startup if missing), `PI_STARTUP_TIMEOUT_SECONDS`
  (default `90` — see "Startup timeout" below).
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

Both are optional; a note without either key resolves to `claude` (the
default engine) unless `DEFAULT_ENGINE`/`REVIEW_ENGINE` or the repo's own
config say otherwise. Two engines are registered today: `claude` and
`pi`. Resolution order, highest precedence first:

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
written back to the note (the reviewer has no session to resume). A task
can code with one engine and be reviewed by another — e.g. `engine:
claude` with `review_engine: pi`.

### The `pi` engine (DeepSeek)

`pi` drives the `pi` coding-agent CLI configured against a DeepSeek model.
Unlike `claude`, this engine carries no credentials or model choice of its
own in `pmrunner`'s config:

- **Credentials** live entirely in `pi`'s own credential store, for
  whatever OS user the runner process runs as. `pmrunner` never passes
  `--api-key` or otherwise handles a DeepSeek key — if `pi auth check
  --provider deepseek` fails for that user, every `pi` task will hang (see
  "Startup timeout" below) until it's fixed outside `pmrunner` entirely.
- **The default model** comes from `pi`'s own settings for that user
  (`~/.pi/agent/settings.json`'s `defaultProvider`/`defaultModel`) unless
  `PI_MODEL`/`PI_REVIEW_MODEL` is set, in which case `pmrunner` passes
  `--model <value>` explicitly. Changing the model without touching
  `pmrunner`'s config at all means editing `pi`'s own settings file.
- **Sessions**: every fresh `pi` coding run gets a freshly minted session
  ID (`rbg-` plus 16 random hex characters — never derived from the task
  slug, so a task note re-filed under the same slug never accidentally
  resumes an old session) and is stored under `PI_SESSION_DIR` as
  `<timestamp>_<id>.jsonl`, same as `pi` always stores its sessions. A
  resume reuses the exact `session_id` the note already recorded.
  `pmrunner` never reads these transcript files itself.
- **Exit code is not trustworthy.** `pi --mode json` still exits `0` on a
  failed or aborted model response — `pmrunner` detects that from the
  JSONL event stream instead (an assistant turn whose `stopReason` is
  `"error"`/`"aborted"`, or an `auto_retry_end` reporting `success: false`)
  and turns it into an ordinary task error either way, never a
  usage-limit pause (DeepSeek quota/balance detection doesn't exist yet).
- **Startup timeout**: a bad DeepSeek credential makes `pi` hang
  completely silently — no stdout, no stderr, no exit. `pi` is the only
  engine that declares a startup timeout (`PI_STARTUP_TIMEOUT_SECONDS`,
  default `90`): if its coding invocation produces no stdout at all within
  that window, the runner kills it and blocks the task with an alert
  naming the timeout and suggesting `pi auth check`. `claude` declares no
  startup timeout and is unaffected; the ordinary idle watchdog
  (`IDLE_TIMEOUT_MINUTES`) still applies to `pi` as well, for a hang any
  time *after* its first line of output.

## HTTP API

The runner exposes a local HTTP surface (bound to `127.0.0.1:$HTTP_PORT`,
default `8420`, no auth). Two of its routes are engine-aware, since a
usage limit belongs to one engine's account and must never stop tasks
running on a different engine:

- **`POST /resume`** — the manual counterpart to the runner's own
  auto-resume timer. An optional `?engine=<name>` query parameter scopes
  the resume to that one engine only (clearing its pause, flipping its
  blocked notes to `blocker_resolved`); omitted, it clears every engine
  currently paused. With only `claude` ever in use, there's at most one
  paused engine at a time, so `POST /resume` with no parameter behaves
  exactly as it always has.
- **`GET /status/tasks`** — per-repo in-flight task info (now including
  each task's resolved `engine`), plus pause visibility:
  - `paused` (bool) and `resets_at` — unchanged meaning: true/earliest
    reset if *any* engine is currently paused.
  - `engines` (new) — a per-engine breakdown, `{"<name>": {"paused": bool,
    "resets_at": ...}}`, covering every registered engine (even one
    that's never been tripped, which reports `{"paused": false}`) plus
    any engine `pause.State` has ever recorded.

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
