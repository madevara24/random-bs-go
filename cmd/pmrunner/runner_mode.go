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
	notifier := &notify.Notifier{Vault: vault, WebhookURL: cfg.DiscordWebhookURL}

	runnerDeps := runner.Deps{
		Vault:       vault,
		Repos:       cfg.Repos,
		IdleTimeout: time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
		OnBlocked: func(payload runner.AlertPayload) {
			notifier.SendBlocked(
				fmt.Sprintf("Tasks/%s.md", payload.Slug), payload.Slug,
				fmt.Sprintf("%s during %s", payload.Scenario, payload.Stage),
				payload.DiscordMessage(cfg.DiscordUserID), payload.Slug+".md", payload.AttachmentMarkdown())
		},
		OnTerminal: func(job worker.Job, status, workLog string) {
			if status == "blocked" {
				notifier.SendBlocked(job.NotePath, job.Slug, "CC itself set status: blocked -- see its Work Log for what it needs",
					fmt.Sprintf("<@%s> Task `%s` (%s) is **blocked** -- see its Work Log.", cfg.DiscordUserID, job.Slug, job.Repo),
					job.Slug+".md", "# Task blocked\n\n"+workLog+"\n")
				return
			}
			notifier.Send(job.NotePath,
				fmt.Sprintf("<@%s> Task `%s` (%s) is **%s**.", cfg.DiscordUserID, job.Slug, job.Repo, status),
				job.Slug+".md", "# Task "+status+"\n\n"+workLog+"\n")
		},
		MergeGateFactory: func(repoCfg config.RepoConfig, job worker.Job, branchName, sessionID, prURL string) runner.MergeGateOps {
			return &runner.GhMergeGateOps{
				RepoPath:      repoCfg.Path,
				Branch:        branchName,
				DefaultBranch: repoCfg.DefaultBranch,
				ClaudeBin:     "claude",
				CodingSession: sessionID,
			}
		},
		OnRoundLimitHit: func(job worker.Job, prURL string) {
			notifier.Send(job.NotePath,
				fmt.Sprintf("<@%s> Task `%s` (%s) hit the review/CI round limit without merging -- PR is still open at %s, needs a human's judgment.", cfg.DiscordUserID, job.Slug, job.Repo, prURL),
				job.Slug+"-round-limit.md", fmt.Sprintf("# Merge-gate round limit hit\n\n- PR: %s\n- Repo: %s\n\nThe review/CI loop used all %d rounds without a clean merge. The PR is left open; status stays \"done\" in the vault note.\n", prURL, job.Repo, runner.MaxMergeGateRounds))
		},
	}

	globalSlots := worker.NewGlobalSlots(cfg.GlobalSlots)
	workers := buildWorkers(cfg, globalSlots, runnerDeps, newPanicHandler(vault, notifier, cfg.DiscordUserID))
	workers.StartAll()

	r := daemon.NewRunner(vault, workers)
	if err := r.Boot(); err != nil {
		fmt.Printf("pmrunner: fatal: boot failed: %v\n", err)
		return
	}

	go r.RunDispatchLoop(func(err error) {
		fmt.Printf("[runner] dispatch pass error: %v\n", err)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.HTTPPort)
	server := httpapi.New(r.DispatchWake, workers, vault)
	fmt.Printf("[runner] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		fmt.Printf("pmrunner: fatal: http server: %v\n", err)
	}
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

// newPanicHandler builds the RepoWorker.OnPanic callback wired into
// production: a Go panic inside ProcessTask means Process never got the
// chance to write anything to the vault note itself, so this is the
// fallback of last resort, same role as setupfallback.go's
// handleSetupFailure for a setup-step error -- write status: blocked with
// the recovered panic value, then always fire the Discord alert regardless
// of whether that write succeeded.
func newPanicHandler(vault *vaultgit.Vault, notifier *notify.Notifier, discordUserID string) func(job worker.Job, recovered any) {
	return func(job worker.Job, recovered any) {
		entry := fmt.Sprintf("%s: recovered from panic in repo %s: %v", time.Now().UTC().Format(time.RFC3339), job.Repo, recovered)
		writeErr := vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (panic)", job.Slug), func(n *notetask.Note) error {
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
		notifier.SendBlocked(job.NotePath, job.Slug,
			fmt.Sprintf("panic recovered in Process: %v", recovered),
			fmt.Sprintf("<@%s> Task `%s` (%s) is **blocked** -- panic recovered: %v", discordUserID, job.Slug, job.Repo, recovered),
			job.Slug+".md",
			fmt.Sprintf("# Task blocked: panic\n\n- **Repo**: %s\n- **Recovered panic**: %v\n", job.Repo, recovered))

		fmt.Printf("[runner] task %s: blocked (panic recovered): %v\n", job.Slug, recovered)
	}
}
