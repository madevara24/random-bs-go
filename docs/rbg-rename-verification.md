# `rbg` rename verification

This is the reviewable evidence for the `full-verify` unit of the program
rename. The old-name spellings are intentionally not repeated in this
committed file so that the repo-wide old-name grep stays clean; the
authoritative old-to-new table lives in the task note's `## Rename map`.

## Goal

Rename the program to `rbg` across the command package, build output,
deploy folder, systemd units, environment variables, test fixtures, CI
identity, note-copy filename, test markers, log prefixes, and docs.
Behaviour is unchanged: only names, paths, and environment-variable names
move.

## Rename map (new names)

- `cmd/rbg/` (moved with `git mv`).
- Binary and build output `rbg`; `.gitignore` ignores `/rbg`.
- Deploy folder `/home/obsidian/rbg`; deploy temp/backup names `.rbg.new`
  and `rbg.prev`.
- systemd units `rbg-runner`, `rbg-watcher`.
- Env vars `RBG_CONFIG_DIR`, `RBG_TEST_VAULT_PATH`,
  `RBG_TEST_TARGET_REPO_PATH`, `RBG_SEND_REAL_DISCORD_TEST`,
  `RBG_RUN_SANDBOX_TEST` (the old names have no fallback).
- Fixture default `/home/obsidian/rbg-test-vault`; CI fixtures
  `/tmp/rbg-test-vault-ci` and `/tmp/rbg-test-target-ci`; CI git identity
  `rbg-ci` / `rbg-ci@users.noreply.github.com`.
- Note copy `.rbg-task-<slug>.md`.
- Test markers `rbg-phase6-test`, `rbg-phase7-test`.
- Log/error prefix `rbg:`; watcher messages and docs use `watcher` and the
  `watcher:` prefix.
- `README.md`, `CLAUDE.md`, `Makefile`, workflows, and code comments use
  the new names.

## Verify results

### Repo-wide old-name sweep

`git grep -i -nE '<old-name-pattern>'` prints nothing. The command's exact
pattern is recorded in the task note. An untracked-file scan (excluding
`.git` and the git-ignored task copy) is also clean.

### Build, vet, lint, binary

- `go build ./...`: clean.
- `go vet ./...`: clean.
- `golangci-lint run` (v2.15.0): clean, `0 issues.`
- `make build`: produces `./rbg`.
- `./rbg` with no arguments prints:

```text
usage: rbg <runner|watcher> [--config-dir=<dir>]
```

### Test-fixture and test-run comparison

Both runs used the repo's own `make test-fixture` followed by
`go test ./... -v`.

- After (this branch): fixtures skipped as already present at
  `/home/obsidian/rbg-test-vault` and `/home/obsidian/repos/phase6-test-repo`;
  results were 333 RUN / 328 PASS / 0 FAIL / 5 SKIP.
- Before (`origin/main` at `b9bed10`, extracted with `git archive` to
  `/tmp/rbg-before`, old fixture-path defaults): fixtures skipped as
  already present; results were 333 RUN / 328 PASS / 0 FAIL / 5 SKIP.

Both SKIP lists, in order:

1. `TestSendAgainstRealWebhook`
2. `TestAlertPayloadReachesDiscordInVerifiedShape`
3. `TestSandboxCleanPass`
4. `TestSandboxConcernsThenFix`
5. `TestSandboxRoundExhaustion`

Only the gating env-var names inside the messages differ between the two
runs (the renamed `RBG_*` variables versus their old names); the rest of
each message is byte-identical. The current skip messages are:

```text
notify_test.go:128: set RBG_SEND_REAL_DISCORD_TEST=1 to actually fire this against the real Discord webhook
discord_shape_test.go:27: set RBG_SEND_REAL_DISCORD_TEST=1 to actually fire this against the real Discord webhook
mergegate_sandbox_test.go:120: set RBG_RUN_SANDBOX_TEST=1 to run real scenarios against madevara24/sandbox
mergegate_sandbox_test.go:187: set RBG_RUN_SANDBOX_TEST=1 to run real scenarios against madevara24/sandbox
mergegate_sandbox_test.go:255: set RBG_RUN_SANDBOX_TEST=1 to run real scenarios against madevara24/sandbox
```

### Deploy dry-run

`make deploy DRY_RUN=1` output:

```text
go build -o rbg ./cmd/rbg
deploy: installing rbg into /home/obsidian/rbg
[dry-run] cp rbg /home/obsidian/rbg/.rbg.new
[dry-run] cp -a /home/obsidian/rbg/rbg /home/obsidian/rbg/rbg.prev
[dry-run] chmod 755 /home/obsidian/rbg/.rbg.new
[dry-run] mv /home/obsidian/rbg/.rbg.new /home/obsidian/rbg/rbg
[dry-run] sudo -n systemctl restart rbg-runner rbg-watcher
```

Only `/home/obsidian/rbg` and `rbg-runner`/`rbg-watcher` appear.

## Compatibility

- **Behaviour unchanged:** only names, paths, and env vars changed. The
  before/after test counts and SKIP test names are identical.
- **In-flight note copies:** a copy written under the old name would not
  be read back, so the human merges only while no task is running.
- **Stale old copies:** old-name task files left in target clones
  are never referenced; the runner reads and writes only
  `.rbg-task-<slug>.md`.
- **Deploy units:** the deploy workflow restarts `rbg-runner` and
  `rbg-watcher`; `/home/obsidian/rbg`, those units, and the sudoers rule
  are created by the human before merge, so the merge waits for that step.
  Post-merge checks are a green Deploy run, `/health` printing `ok`, and
  both units reporting `active`.

## Environment note

`golangci-lint` was installed under `/home/obsidian/go/bin` but was not on
the runner's systemd `PATH`. A user-level symlink at
`/home/obsidian/.local/bin/golangci-lint` (already on that `PATH`) makes
the verify command work. No repo or systemd change was made.
