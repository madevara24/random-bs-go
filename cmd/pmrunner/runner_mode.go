package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/daemon"
	"github.com/madevara24/random-bs-go/internal/httpapi"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/runner"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// runRunner is the entrypoint for `pmrunner runner`: builds the vault
// handle and one RepoWorker per configured repo, boots (sync + reconcile),
// starts the dispatch-pass loop and the HTTP surface, and blocks forever
// serving HTTP.
func runRunner(cfg *config.Config) {
	fmt.Printf("pmrunner: mode=runner vault=%s repos=%d global_slots=%d idle_timeout=%dm http_port=%d\n",
		cfg.VaultPath, len(cfg.Repos), cfg.GlobalSlots, cfg.IdleTimeoutMinutes, cfg.HTTPPort)

	// This process's raw environment doesn't change after boot, so checking
	// once here is representative of every git subprocess call the daemon
	// will ever make. vaultgit.CleanGitEnv() strips GIT_DIR/GIT_WORK_TREE/
	// GIT_INDEX_FILE unconditionally regardless, but this line answers
	// empirically whether a parent process has leaked those vars into this
	// daemon's environment.
	fmt.Printf("[runner] GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE present in daemon env: %+v\n", vaultgit.EnvSnapshot())

	vault := vaultgit.New(cfg.VaultPath, cfg.VaultDefaultBranch)
	notifier := &notify.Notifier{Vault: vault, WebhookURL: cfg.DiscordWebhookURL, ForumWebhookURL: cfg.DiscordTaskForumWebhookURL}

	// pauseState is the shared pipeline-pause flag -- constructed once here
	// and threaded into runner.Deps (so ProcessTask can trip it), every
	// RepoWorker (so the drain loop stops popping), daemon.NewRunner (so
	// RunDispatchPass stops claiming), and httpapi.New (so GET
	// /status/tasks can report it and POST /resume can clear it). Not
	// persisted across a runner restart -- a fresh process starts unpaused
	// even if the limit is, in reality, still in effect; the next task to
	// hit it re-trips the pause as normal.
	pauseState := &pause.State{}

	// server is assigned after construction below; resumeMgr's timer
	// closure captures this variable (not its value at closure-creation
	// time), so by the time the timer actually fires -- long after boot --
	// it calls the real server's Resume, which does the actual clear-pause
	// +flip-notes+wake-dispatch+wake-workers mechanics.
	var server *httpapi.Server
	resumeMgr := newResumeTimerMgr(cfg, notifier, func() ([]string, error) {
		return server.Resume()
	})

	var postClaim func(threadName, content string) (string, error)
	if cfg.DiscordTaskForumWebhookURL != "" {
		postClaim = func(threadName, content string) (string, error) {
			return notify.PostForumClaim(cfg.DiscordTaskForumWebhookURL, threadName, content)
		}
	}

	runnerDeps := runner.Deps{
		Vault:       vault,
		Repos:       cfg.Repos,
		IdleTimeout: time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
		PostClaim:   postClaim,
		Pause:       pauseState,
		OnBlocked: func(job worker.Job, payload runner.AlertPayload) {
			notifier.Send(
				job.NotePath, payload.DiscordThreadID,
				payload.DiscordMessage(cfg.DiscordUserID, cfg.DiscordAraDevUserID), payload.Slug+".md", payload.AttachmentMarkdown())
			if payload.Scenario == runner.ScenarioUsageLimit {
				resumeMgr.handleTrip(resetsAtOf(payload.UsageLimit))
			}
		},
		OnTerminal: newTerminalHandler(cfg, notifier, resumeMgr),
		MergeGateFactory: func(repoCfg config.RepoConfig, job worker.Job, branchName, sessionID, prURL string) runner.MergeGateOps {
			return &runner.GhMergeGateOps{
				RepoPath:      repoCfg.Path,
				PRURL:         prURL,
				DefaultBranch: repoCfg.DefaultBranch,
				ClaudeBin:     "claude",
				CodingSession: sessionID,
			}
		},
		OnRoundLimitHit: func(job worker.Job, prURL, discordThreadID string) {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) hit the review/CI round limit without merging -- PR is still open at %s, needs a human's judgment.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, prURL),
				job.Slug+"-round-limit.md", fmt.Sprintf("# Merge-gate round limit hit\n\n- PR: %s\n- Repo: %s\n\nThe review/CI loop used all %d rounds without a clean merge. The PR is left open; status stays \"done\" in the vault note.\n", prURL, job.Repo, runner.MaxMergeGateRounds))
		},
		OnMergeGateError: func(job worker.Job, prURL, discordThreadID string, err error) {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) hit an error in the review/CI merge-gate loop -- PR is still open at %s, needs a human's judgment.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, prURL),
				job.Slug+"-mergegate-error.md", fmt.Sprintf("# Merge-gate loop error\n\n- PR: %s\n- Repo: %s\n- Error: %v\n\nThe review/CI loop ended with an error before reaching a clean merge or exhausting its round budget. The PR is left open; status stays \"done\" in the vault note.\n", prURL, job.Repo, err))
		},
	}

	globalSlots := worker.NewGlobalSlots(cfg.GlobalSlots)
	workers := buildWorkers(cfg, globalSlots, runnerDeps, newPanicHandler(vault, notifier, cfg.DiscordUserID, cfg.DiscordAraDevUserID))
	for _, rw := range workers {
		rw.Pause = pauseState
	}
	workers.StartAll()

	r := daemon.NewRunner(vault, workers, pauseState)
	if err := r.Boot(); err != nil {
		fmt.Printf("pmrunner: fatal: boot failed: %v\n", err)
		return
	}

	go r.RunDispatchLoop(func(err error) {
		fmt.Printf("[runner] dispatch pass error: %v\n", err)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.HTTPPort)
	server = httpapi.New(r.DispatchWake, workers, vault, pauseState)
	server.CancelResumeTimer = func() { resumeMgr.cancel() }
	fmt.Printf("[runner] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		fmt.Printf("pmrunner: fatal: http server: %v\n", err)
	}
}

// resetsAtOf is a nil-safe accessor for AlertPayload.UsageLimit.ResetsAt --
// used wherever resumeMgr.handleTrip needs a *time.Time regardless of
// whether UsageLimit itself is nil.
func resetsAtOf(u *runner.UsageLimitInfo) *time.Time {
	if u == nil {
		return nil
	}
	return u.ResetsAt
}

// buildWorkers constructs one RepoWorker per configured repo, wiring
// Process to runner.ProcessTask and OnPanic to onPanic -- split out of
// runRunner so a test can assert every worker comes out of construction
// with both callbacks wired, without booting the full daemon (HTTP server,
// dispatch loop, real vault sync).
func buildWorkers(cfg *config.Config, globalSlots chan struct{}, runnerDeps runner.Deps, onPanic func(job worker.Job, recovered any)) worker.Workers {
	workers := worker.Workers{}
	for key := range cfg.Repos {
		rw := worker.New(key, globalSlots, nil)
		rw.Process = func(job worker.Job) error {
			return runner.ProcessTask(runnerDeps, rw, job)
		}
		rw.OnPanic = onPanic
		workers[key] = rw
	}
	return workers
}

// newTerminalHandler builds the runner.Deps.OnTerminal callback wired into
// production. A `done` never attaches the task's .md file -- the PR is the
// deliverable, not the work log -- and its message wording branches on
// autoMerge: manual-merge wording when false, auto-merge-loop wording when
// true. OnTerminal fires before the merge-gate loop runs (see
// runMergeGateForTask in internal/runner/runner.go), so a done+auto_merge
// message can only say the loop is running, never that the PR merged. A
// `done` message mentions only Devara, not Ara-Dev: the Ara-Dev mention
// exists so the `dev` Hermes gateway auto-threads a reply to help unblock,
// and a done task needs no help. `blocked` and `failed` both keep the .md
// attachment so the work log is there to see what went wrong, and both
// keep mentioning Ara-Dev -- except a `blocked` whose usageLimit is
// non-nil: that's the "self-reported blocked while a limit is tripped"
// case (this very invocation hit the usage limit, but CC still got a
// valid copy write in with its own status: blocked before claude exited),
// which mentions Devara only, same reasoning as a crash-detected
// ScenarioUsageLimit block -- there's nothing for Ara-Dev's gateway to
// unblock, the cause clears on its own -- and also arms/re-arms resumeMgr's
// auto-resume timer, the same hook OnBlocked's ScenarioUsageLimit case
// uses.
func newTerminalHandler(cfg *config.Config, notifier *notify.Notifier, resumeMgr *resumeTimerMgr) func(job worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *runner.UsageLimitInfo) {
	return func(job worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *runner.UsageLimitInfo) {
		if status == "blocked" && usageLimit != nil {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> Task `%s` (%s) hit the Claude usage limit -- the pipeline is **paused** until it resets%s. See its Work Log.",
					cfg.DiscordUserID, job.Slug, job.Repo, resetsAtSuffix(usageLimit)),
				job.Slug+".md", "# Task blocked: usage limit\n\n"+workLog+"\n")
			resumeMgr.handleTrip(usageLimit.ResetsAt)
			return
		}
		if status == "blocked" {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **blocked** -- see its Work Log.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo),
				job.Slug+".md", "# Task blocked\n\n"+workLog+"\n")
			return
		}
		if status == "done" {
			mergeNote := "the PR needs a manual merge"
			if autoMerge {
				mergeNote = "the auto-merge loop is running"
			}
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> Task `%s` (%s) is **done**: %s -- %s.", cfg.DiscordUserID, job.Slug, job.Repo, prURL, mergeNote),
				"", "")
			return
		}
		notifier.Send(job.NotePath, discordThreadID,
			fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **%s**.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, status),
			job.Slug+".md", "# Task "+status+"\n\n"+workLog+"\n")
	}
}

// resetsAtSuffix is newTerminalHandler's " (resets at ...)" clause for a
// usage-limit block, or "" if the reset time is unknown -- mirrors
// crashfallback.go's own resetsAtSuffix for AlertPayload, kept as a
// separate copy since that one is unexported in a different package.
func resetsAtSuffix(u *runner.UsageLimitInfo) string {
	if u == nil || u.ResetsAt == nil {
		return ""
	}
	return fmt.Sprintf(" (resets at %s)", u.ResetsAt.UTC().Format(time.RFC3339))
}

// newPanicHandler builds the RepoWorker.OnPanic callback wired into
// production: a Go panic inside ProcessTask means Process never got the
// chance to write anything to the vault note itself, so this is the
// fallback of last resort, same role as setupfallback.go's
// handleSetupFailure for a setup-step error -- write status: blocked with
// the recovered panic value, then always fire the Discord alert regardless
// of whether that write succeeded.
func newPanicHandler(vault *vaultgit.Vault, notifier *notify.Notifier, discordUserID, araDevUserID string) func(job worker.Job, recovered any) {
	return func(job worker.Job, recovered any) {
		entry := fmt.Sprintf("%s: recovered from panic in repo %s: %v", time.Now().UTC().Format(time.RFC3339), job.Repo, recovered)
		var discordThreadID string
		writeErr := vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (panic)", job.Slug), func(n *notetask.Note) error {
			if n.Frontmatter.DiscordThreadID != nil {
				discordThreadID = *n.Frontmatter.DiscordThreadID
			}
			n.Frontmatter.Status = "blocked"
			if strings.TrimSpace(n.WorkLog) == "" {
				n.WorkLog = entry
			} else {
				n.WorkLog = strings.TrimSpace(n.WorkLog) + "\n" + entry
			}
			n.HasWorkLog = true
			notetask.AppendRunnerLog(n, "blocked", time.Now())
			return nil
		})
		if writeErr != nil {
			fmt.Printf("[runner] task %s: failed to write blocked status after panic: %v\n", job.Slug, writeErr)
		}

		// Always fires, independent of writeErr above -- Discord must not
		// depend on the vault being writable, same rule as
		// handleSetupFailure's Discord call.
		notifier.Send(job.NotePath, discordThreadID,
			fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **blocked** -- panic recovered: %v", discordUserID, araDevUserID, job.Slug, job.Repo, recovered),
			job.Slug+".md",
			fmt.Sprintf("# Task blocked: panic\n\n- **Repo**: %s\n- **Recovered panic**: %v\n", job.Repo, recovered))

		fmt.Printf("[runner] task %s: blocked (panic recovered): %v\n", job.Slug, recovered)
	}
}
