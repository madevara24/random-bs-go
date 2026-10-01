package main

import (
	"fmt"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/watcher"
)

// pollInterval is how often pmwatch runs its three checks. 60s is a
// reasonable default -- cheap enough not to matter and short enough to
// catch a dead pipeline promptly.
const pollInterval = 60 * time.Second

// healthAlertThreshold is how many consecutive failed /health cycles must
// happen before the watcher sends a webhook. A deploy restarts
// pmrunner-runner and pmrunner-watcher together, so the watcher's very
// first cycle routinely loses the race against the runner binding its
// port; 3 cycles (3 minutes at pollInterval=60s) rides that out without
// staying quiet through a real outage for long.
const healthAlertThreshold = 3

// healthAlertDebouncer tracks consecutive Tier 1 /health failures across
// watch cycles and decides when the failure streak is long enough to
// alert. It holds no notifier or HTTP client, so it stays trivially
// testable independent of how the alert is actually sent.
type healthAlertDebouncer struct {
	consecutiveFailures int
}

// Observe records one cycle's health result and reports whether this
// cycle should alert. It returns true exactly once per failure streak --
// on the cycle where consecutive failures first reach
// healthAlertThreshold -- and resets the streak to zero on HealthOK.
func (d *healthAlertDebouncer) Observe(health watcher.HealthResult) bool {
	if health == watcher.HealthOK {
		d.consecutiveFailures = 0
		return false
	}
	d.consecutiveFailures++
	return d.consecutiveFailures == healthAlertThreshold
}

// runWatcher is the entrypoint for `pmrunner watcher` (pmwatch): runs three
// checks every pollInterval, forever -- Tier 1 /health, Tier 2
// /status/tasks (only if Tier 1 succeeded), and an independent direct
// Tasks/ scan for notes that reached a terminal status with no
// notification ever sent.
func runWatcher(cfg *config.Config) {
	fmt.Printf("pmrunner: mode=watcher vault=%s idle_timeout=%dm http_port=%d\n",
		cfg.VaultPath, cfg.IdleTimeoutMinutes, cfg.HTTPPort)

	vault := vaultgit.New(cfg.VaultPath, cfg.VaultDefaultBranch)
	notifier := &notify.Notifier{
		RunnerLogURL:    fmt.Sprintf("http://127.0.0.1:%d/runner-log", cfg.HTTPPort),
		WebhookURL:      cfg.DiscordWebhookURL,
		ForumWebhookURL: cfg.DiscordTaskForumWebhookURL,
	}

	w := &watcher.Watcher{
		BaseURL:     fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort),
		Vault:       vault,
		IdleTimeout: time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
	}

	healthAlerts := &healthAlertDebouncer{}
	for {
		runOneWatchCycle(w, notifier, cfg, healthAlerts)
		time.Sleep(pollInterval)
	}
}

func runOneWatchCycle(w *watcher.Watcher, notifier *notify.Notifier, cfg *config.Config, healthAlerts *healthAlertDebouncer) {
	health, err := w.CheckHealth()
	if health != watcher.HealthOK {
		fmt.Printf("[watcher] /health check failed: %v (%v)\n", health, err)
		if healthAlerts.Observe(health) {
			notifier.Send("", "", fmt.Sprintf("<@%s> <@%s> pmwatch: runner daemon health check failed -- %v (%v)", cfg.DiscordUserID, cfg.DiscordAraDevUserID, health, err),
				"health-check.md", fmt.Sprintf("# Runner health check failed\n\n- Result: %v\n- Error: %v\n", health, err))
		}
	} else {
		healthAlerts.Observe(health)
		_, stale, err := w.CheckStatusTasks()
		if err != nil {
			fmt.Printf("[watcher] /status/tasks check failed: %v\n", err)
		}
		for _, s := range stale {
			fmt.Printf("[watcher] stale task detected: repo=%s slug=%s idle_for=%v\n", s.RepoKey, s.Slug, s.IdleFor)
			notifier.Send("", s.DiscordThreadID, fmt.Sprintf("<@%s> <@%s> pmwatch: task `%s` (%s) looks wedged -- idle for %v.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, s.Slug, s.RepoKey, s.IdleFor),
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
		notifier.Send(u.RelPath, u.DiscordThreadID, fmt.Sprintf("<@%s> <@%s> pmwatch: task `%s` reached status `%s` %v ago with no Discord notification ever recorded.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, u.Slug, u.Status, u.Age),
			u.Slug+"-unnotified.md", fmt.Sprintf("# Lost notification detected\n\n- Note: %s\n- Status: %s\n- Age: %v\n", u.RelPath, u.Status, u.Age))
	}
}
