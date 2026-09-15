package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/daemon"
	"github.com/madevara24/random-bs-go/internal/httpapi"
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

	// Phase 5's empirical GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE check: this
	// process's raw environment doesn't change after boot, so checking once
	// here is representative of every git subprocess call the daemon will
	// ever make (vaultgit.CleanGitEnv() strips these unconditionally
	// regardless, but this line is what actually answers the "did the
	// hazard survive the new architecture" question empirically, per
	// Design - Runner.md's GIT_DIR section).
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
	}

	globalSlots := worker.NewGlobalSlots(cfg.GlobalSlots)
	workers := worker.Workers{}
	for key := range cfg.Repos {
		rw := worker.New(key, globalSlots, nil)
		rw.Process = func(job worker.Job) error {
			return runner.ProcessTask(runnerDeps, rw, job)
		}
		workers[key] = rw
	}
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
	server := httpapi.New(r.DispatchWake)
	fmt.Printf("[runner] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		fmt.Printf("pmrunner: fatal: http server: %v\n", err)
	}
}
