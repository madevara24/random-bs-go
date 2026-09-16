package main

import (
	"fmt"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/watcher"
)

// pollInterval is how often pmwatch runs its three checks. Not pinned by
// any of the design docs (Design - Watcher.md leaves the idle-timeout
// value itself "TBD," and never states a separate poll cadence) -- 60s is
// a reasonable default, cheap enough not to matter and short enough to
// catch a dead pipeline promptly.
const pollInterval = 60 * time.Second

// runWatcher is the entrypoint for `pmrunner watcher` (pmwatch): the real
// polling loop from Design - Watcher.md -- Tier 1 /health, Tier 2
// /status/tasks (only if Tier 1 succeeded), and an independent direct
// Tasks/ scan for gap 3, every pollInterval, forever.
func runWatcher(cfg *config.Config) {
	fmt.Printf("pmrunner: mode=watcher vault=%s idle_timeout=%dm http_port=%d\n",
		cfg.VaultPath, cfg.IdleTimeoutMinutes, cfg.HTTPPort)

	vault := vaultgit.New(cfg.VaultPath, cfg.VaultDefaultBranch)
	notifier := &notify.Notifier{
		RunnerLogURL: fmt.Sprintf("http://127.0.0.1:%d/runner-log", cfg.HTTPPort),
		WebhookURL:   cfg.DiscordWebhookURL,
	}

	w := &watcher.Watcher{
		BaseURL:     fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort),
		Vault:       vault,
		IdleTimeout: time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
	}

	for {
		runOneWatchCycle(w, notifier, cfg)
		time.Sleep(pollInterval)
	}
}

func runOneWatchCycle(w *watcher.Watcher, notifier *notify.Notifier, cfg *config.Config) {
	health, err := w.CheckHealth()
	if health != watcher.HealthOK {
		fmt.Printf("[watcher] /health check failed: %v (%v)\n", health, err)
		notifier.Send("", fmt.Sprintf("<@%s> pmwatch: runner daemon health check failed -- %v (%v)", cfg.DiscordUserID, health, err),
			"health-check.md", fmt.Sprintf("# Runner health check failed\n\n- Result: %v\n- Error: %v\n", health, err))
	} else {
		_, stale, err := w.CheckStatusTasks()
		if err != nil {
			fmt.Printf("[watcher] /status/tasks check failed: %v\n", err)
		}
		for _, s := range stale {
			fmt.Printf("[watcher] stale task detected: repo=%s slug=%s idle_for=%v\n", s.RepoKey, s.Slug, s.IdleFor)
			notifier.Send("", fmt.Sprintf("<@%s> pmwatch: task `%s` (%s) looks wedged -- idle for %v.", cfg.DiscordUserID, s.Slug, s.RepoKey, s.IdleFor),
				s.Slug+"-stale.md", fmt.Sprintf("# Stale task detected\n\n- Repo: %s\n- Slug: %s\n- Stage: %s\n- Idle for: %v\n", s.RepoKey, s.Slug, s.Stage, s.IdleFor))
		}
	}

	unnotified, err := w.ScanForUnnotified(w.IdleTimeout)
	if err != nil {
		fmt.Printf("[watcher] Tasks/ scan for unnotified notes failed: %v\n", err)
		return
	}
	for _, u := range unnotified {
		fmt.Printf("[watcher] unnotified terminal note detected: %s status=%s age=%v\n", u.RelPath, u.Status, u.Age)
		notifier.Send(u.RelPath, fmt.Sprintf("<@%s> pmwatch: task `%s` reached status `%s` %v ago with no Discord notification ever recorded.", cfg.DiscordUserID, u.Slug, u.Status, u.Age),
			u.Slug+"-unnotified.md", fmt.Sprintf("# Lost notification detected\n\n- Note: %s\n- Status: %s\n- Age: %v\n", u.RelPath, u.Status, u.Age))
	}
}
