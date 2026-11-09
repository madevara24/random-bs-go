# rbg

`rbg` turns task notes in a PM vault (a git repo of Markdown files) into coding-agent sessions and pull requests against onboarded repos. It is one Go binary with two modes:

- **runner**: the long-lived daemon. It claims notes at `ready`, `blocker_resolved`, `plan_approved`, or `plan_rejected`, checks out `task/<slug>` in the target repo, runs the task's coding engine (`claude` or `pi`) under an idle watchdog, and writes the result back to the note. For `auto_merge: true` tasks it then runs a review and CI merge gate and merges the PR.
- **watcher**: a separate polling process. It checks the runner's health, flags wedged tasks, and catches any finished note that never got its Discord notice.

A task too large for one session can run as a **multi-unit task**: a planner splits it into units, each unit gets a fresh coding session and its own review, and the runner opens one PR at the end. See [Multi-unit tasks](#multi-unit-tasks).

## Build

```
make build
```

This runs `go build -o rbg ./cmd/rbg`.

## Run

```
./rbg runner  [--config-dir=<dir>]
./rbg watcher [--config-dir=<dir>]
```

The config directory is `--config-dir`, else `$RBG_CONFIG_DIR`, else the current directory. Both modes read the same `.env` and `repos.json` from it.

## Config

### `.env`

Plain `KEY=VALUE` lines. Matched surrounding quotes are stripped. There is no other escaping. A missing required key stops startup with an error that names it.

Required:

| Key | Meaning |
|---|---|
| `DISCORD_WEBHOOK_URL` | Webhook for top-level alerts |
| `DISCORD_USER_ID` | User mentioned on every alert |
| `DISCORD_ARA_DEV_USER_ID` | User also mentioned on `blocked` and `failed` alerts |
| `GLOBAL_SLOTS` | Positive integer: how many tasks run at once, across all repos |
| `IDLE_TIMEOUT_MINUTES` | Positive integer: minutes of silence before the watchdog kills a session. The watcher uses the same value as its staleness threshold. |
| `VAULT_PATH` | The runner's own working clone of the PM vault |
| `VAULT_DEFAULT_BRANCH` | The vault branch the runner syncs to |

Optional:

| Key | Default | Meaning |
|---|---|---|
| `HTTP_PORT` | `8420` | Port for the local HTTP API |
| `DISCORD_TASK_FORUM_WEBHOOK_URL` | unset | Forum webhook for per-task threads. Unset: every message goes to the top-level webhook, and the plan-review poller does not start. |
| `USAGE_LIMIT_RESUME_GRACE_SECONDS` | `60` | Wait after a usage limit's reset time before tasks resume |
| `DEFAULT_ENGINE` | `claude` | Coding engine |
| `REVIEW_ENGINE` | `claude` | Engine for reviews and the merge gate |
| `PLAN_ENGINE` | `claude` | Engine for the multi-unit planner |
| `CLAUDE_BIN` | `claude` | Path or name of the `claude` CLI |
| `MULTI_UNIT_ENABLED` | `false` | Turns on triage and the multi-unit path |
| `TYPESAFE_API_KEY` | unset | Key for Jev, the classifier behind triage and the plan gate |
| `JEV_MODEL` | `jev-latest` | Jev model name |
| `PI_BIN` | `pi` | Path or name of the `pi` CLI |
| `PI_MODEL` | unset | Passed as `--model` to coding runs. Unset: `pi` uses its own settings. |
| `PI_REVIEW_MODEL` | `PI_MODEL` | Passed as `--model` to review runs |
| `PI_SESSION_DIR` | `pi-sessions` in the config directory | Where `pi` stores sessions. Created at startup if missing. |
| `PI_STARTUP_TIMEOUT_SECONDS` | `90` | See [The `pi` engine](#the-pi-engine-deepseek) |

Every engine name (`DEFAULT_ENGINE`, `REVIEW_ENGINE`, `PLAN_ENGINE`, and the per-repo fields below) must be `claude` or `pi`. Any other name stops startup.

### `repos.json`

A map of repo key to onboarding config:

```json
{
  "my-repo": {
    "path": "/path/to/local/clone",
    "remote": "git@github.com:org/my-repo.git",
    "default_branch": "main",
    "auto_merge_default": false,
    "engine": "claude",
    "review_engine": "claude",
    "plan_engine": "claude"
  }
}
```

`path`, `remote`, and `default_branch` are required. `auto_merge_default` applies when a note sets no `auto_merge`. The three engine fields are optional per-repo overrides.

### Engine selection

A task note can pin its engines in frontmatter:

```yaml
engine: pi
review_engine: claude
plan_engine: claude
```

Each role resolves on its own, highest precedence first: the note's field, then the repo's field in `repos.json`, then the `.env` default.

| Role | Note field | Repo field | `.env` default |
|---|---|---|---|
| Coding | `engine` | `engine` | `DEFAULT_ENGINE` |
| Review | `review_engine` | `review_engine` | `REVIEW_ENGINE` |
| Planning (multi-unit only) | `plan_engine` | `plan_engine` | `PLAN_ENGINE` |

The coding engine is resolved when the run starts and written into the note's `engine:`. Until then, an `engine:` key on a note means the author set it. A resume always reuses that engine, because one engine's `session_id` means nothing to another. The review engine is resolved each time a review starts and is never written back.

### The `pi` engine (DeepSeek)

`pi` drives the `pi` coding-agent CLI against a DeepSeek model. `rbg` holds no DeepSeek credentials or model choice of its own:

- **Credentials** live in `pi`'s own store, for the OS user the runner runs as. If `pi auth check --provider deepseek` fails for that user, every `pi` task hangs until the startup timeout fires.
- **The model** comes from `pi`'s settings (`~/.pi/agent/settings.json`, `defaultProvider` and `defaultModel`) unless `PI_MODEL` or `PI_REVIEW_MODEL` is set.
- **Sessions:** every fresh run gets a new id, `rbg-` plus 16 random hex characters, never derived from the task slug. A note filed again under the same slug therefore never resumes an old session. A resume reuses the `session_id` the note recorded.
- **The exit code is not trusted.** `pi --mode json` exits `0` even on a failed model response. `rbg` reads the JSONL events instead: an assistant turn whose `stopReason` is `error` or `aborted`, or an `auto_retry_end` with `success: false`, fails the task. A DeepSeek quota failure is an ordinary task error, never a usage-limit pause.
- **Startup timeout:** a bad DeepSeek credential makes `pi` hang with no output and no exit. If a `pi` coding run prints nothing within `PI_STARTUP_TIMEOUT_SECONDS`, the runner kills it and blocks the task with an alert that suggests `pi auth check`. The idle watchdog still covers a hang after the first line of output.

## Multi-unit tasks

Off unless `MULTI_UNIT_ENABLED=true`.

1. **Triage.** On a fresh run with `multi_unit` unset, Jev answers one question: is this too large for one session? A score of 0.5 or higher picks multi-unit. Anything lower, or any Jev error, picks the single-session path. The answer is stored in `multi_unit`. An author can set `multi_unit` to skip triage.
2. **Planner.** A `plan_engine` session reads the code and writes `## Plan` into the note: a summary plus one fenced `yaml` block of units (`id`, `title`, `paths`, `depends_on`, `accept`, `verify`).
3. **Plan gate.** The plan stops for a human if a unit path contains `migrations/`, `.github/workflows/`, or `Dockerfile`, if there are more than 8 units, if Jev rates it risky or ambiguous, if Jev can't settle one of the planner's own questions, or if Jev is unavailable. Otherwise the units start at once.
4. **Approval.** A stopped plan is posted to the task's forum thread, and the note moves to `plan_review`. ✅ on that message approves it. ❌, plus feedback written under the current `### Round <n>` in `## Plan Review`, rejects it and triggers a replan. A poller checks once a minute. The fourth plan round blocks the task.
5. **Units.** Each unit gets a fresh coding session, its `verify` command, and a review of its own diff. Limits: 3 review rounds per unit, and `2 × units` rounds per task.
6. **Finish.** The runner pushes the branch and opens one PR. With `auto_merge` on, the merge gate runs an integration check: the reviewer runs the full test suite and judges how the units fit together.

Every failure ends at `blocked` with an alert. Setting the note to `blocker_resolved` resumes at `current_unit`. Finished units never run again.

## HTTP API

Bound to `127.0.0.1:$HTTP_PORT`, with no auth.

| Route | Method | Purpose |
|---|---|---|
| `/dispatch` | POST | Start a dispatch pass. Replies `202` at once. |
| `/health` | GET | Liveness only. Always `ok`, and takes no locks. |
| `/status/tasks` | GET | In-flight tasks per repo, with each task's `engine` and `stage`, plus usage-limit pause state |
| `/runner-log` | POST | Append a Runner Log line to a note through the runner's own vault clone (used by the watcher) |
| `/resume` | POST | Clear a usage-limit pause. `?engine=<name>` clears one engine. Without it, every paused engine clears. |

A usage limit pauses only the engine that hit it. `/status/tasks` reports this two ways:

- `paused` and `resets_at`: true, with the earliest reset, if any engine is paused.
- `engines`: `{"<name>": {"paused": bool, "resets_at": ...}}` for every registered engine.

## Test

```
make test-fixture
go test ./... -v
```

Most integration tests in `internal/runner`, `internal/watcher`, and `internal/vaultgit` need two throwaway git fixtures on disk. `make test-fixture` builds them at `RBG_TEST_VAULT_PATH` and `RBG_TEST_TARGET_REPO_PATH`. When a fixture is missing, those tests **skip** instead of failing, so a green run on a fresh checkout can mean almost nothing ran. Check the `--- SKIP` lines before trusting the result.

`internal/notify`'s real-webhook test also skips unless `RBG_SEND_REAL_DISCORD_TEST=1` is set, because it sends a real Discord message.
