package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/daemon"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/engineresolve"
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

	// pauseState is the shared pipeline-pause flag, keyed by engine name --
	// constructed once here and threaded into runner.Deps (so ProcessTask
	// can trip a specific engine), every RepoWorker (so the drain loop
	// stops popping that engine's jobs), daemon.NewRunner (so
	// RunDispatchPass stops claiming that engine's notes), and httpapi.New
	// (so GET /status/tasks can report it and POST /resume can clear it).
	// Not persisted across a runner restart -- a fresh process starts
	// every engine unpaused even if a limit is, in reality, still in
	// effect; the next task to hit it re-trips that engine's pause as
	// normal.
	pauseState := &pause.State{}

	// server is assigned after construction below; resumeMgr's timer
	// closure captures this variable (not its value at closure-creation
	// time), so by the time a given engine's timer actually fires -- long
	// after boot -- it calls the real server's Resume for that engine,
	// which does the actual clear-pause+flip-notes+wake-dispatch+wake-
	// workers mechanics.
	var server *httpapi.Server
	resumeMgr := newResumeTimerMgr(cfg, notifier, func(engine string) ([]string, error) {
		return server.Resume(engine)
	})

	var postClaim func(threadName, content string) (string, error)
	if cfg.DiscordTaskForumWebhookURL != "" {
		postClaim = func(threadName, content string) (string, error) {
			return notify.PostForumClaim(cfg.DiscordTaskForumWebhookURL, threadName, content)
		}
	}

	// PI_SESSION_DIR must exist before pi's own --session-dir ever points
	// at it -- created once here at startup, never inside engine.NewPi
	// itself, since that constructor is also called with empty arguments
	// just to list registered engine names (registeredEngineNames below)
	// and must stay I/O-free.
	if err := os.MkdirAll(cfg.PiSessionDir, 0o755); err != nil {
		fmt.Printf("pmrunner: fatal: creating pi session dir %s: %v\n", cfg.PiSessionDir, err)
		return
	}

	// Built once, shared by every task's ProcessTask/MergeGateFactory call
	// below -- the same registry, not a fresh one per task.
	engineRegistry := buildEngineRegistry(cfg.ClaudeBin, cfg.PiBin, cfg.PiModel, cfg.PiReviewModel, cfg.PiSessionDir, time.Duration(cfg.PiStartupTimeoutSeconds)*time.Second)

	runnerDeps := runner.Deps{
		Vault:               vault,
		Repos:               cfg.Repos,
		EngineRegistry:      engineRegistry,
		ReviewEngineDefault: cfg.ReviewEngine,
		IdleTimeout:         time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
		PostClaim:           postClaim,
		Pause:               pauseState,
		OnBlocked: func(job worker.Job, payload runner.AlertPayload) {
			notifier.Send(
				job.NotePath, payload.DiscordThreadID,
				payload.DiscordMessage(cfg.DiscordUserID, cfg.DiscordAraDevUserID), payload.Slug+".md", payload.AttachmentMarkdown())
			if payload.Scenario == runner.ScenarioUsageLimit {
				resumeMgr.handleTrip(payload.EngineName, resetsAtOf(payload.UsageLimit))
			}
		},
		OnTerminal: newTerminalHandler(cfg, notifier, resumeMgr),
		MergeGateFactory: func(repoCfg config.RepoConfig, job worker.Job, branchName, sessionID, prURL string, codingEngine, reviewEngine engine.Engine) runner.MergeGateOps {
			return &runner.GhMergeGateOps{
				RepoPath:      repoCfg.Path,
				PRURL:         prURL,
				DefaultBranch: repoCfg.DefaultBranch,
				CodingEngine:  codingEngine,
				ReviewEngine:  reviewEngine,
				CodingSession: sessionID,
			}
		},
		OnRoundLimitHit: func(job worker.Job, prURL, discordThreadID string) {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) hit the review/CI round limit without merging -- PR is still open at %s, needs a human's judgment.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, prURL),
				job.Slug+"-round-limit.md", fmt.Sprintf("# Merge-gate round limit hit\n\n- PR: %s\n- Repo: %s\n\nThe review/CI loop used all %d rounds without a clean merge. The PR is left open; status stays \"done\" in the vault note.\n", prURL, job.Repo, runner.MaxMergeGateRounds))
		},
		OnMergeGateError: newMergeGateErrorHandler(cfg, notifier),
	}

	globalSlots := worker.NewGlobalSlots(cfg.GlobalSlots)
	workers := buildWorkers(cfg, globalSlots, runnerDeps, newPanicHandler(vault, notifier, cfg.DiscordUserID, cfg.DiscordAraDevUserID))
	for _, rw := range workers {
		rw.Pause = pauseState
	}
	workers.StartAll()

	resolveEngine := func(repoKey, noteEngine string) string {
		return engineresolve.Coding(noteEngine, cfg.Repos[repoKey].Engine, cfg.DefaultEngine)
	}
	r := daemon.NewRunner(vault, workers, pauseState, resolveEngine)
	if err := r.Boot(); err != nil {
		fmt.Printf("pmrunner: fatal: boot failed: %v\n", err)
		return
	}

	go r.RunDispatchLoop(func(err error) {
		fmt.Printf("[runner] dispatch pass error: %v\n", err)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.HTTPPort)
	server = httpapi.New(r.DispatchWake, workers, vault, pauseState)
	server.EngineNames = registeredEngineNames()
	server.CancelResumeTimer = func(engine string) { resumeMgr.cancel(engine) }
	fmt.Printf("[runner] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		fmt.Printf("pmrunner: fatal: http server: %v\n", err)
	}
}

// buildEngineRegistry builds the name->Engine map runner.Deps/
// MergeGateFactory resolve against -- claude and pi. claudeBin is
// CLAUDE_BIN's resolved value ("" from config.Load means "claude",
// resolved via PATH, same as always); piBin/piModel/piReviewModel/
// piSessionDir/piStartupTimeout are PI_BIN/PI_MODEL/PI_REVIEW_MODEL/
// PI_SESSION_DIR/PI_STARTUP_TIMEOUT_SECONDS's resolved values. Must do no
// I/O: registeredEngineNames below calls this with every argument empty
// just to list names, so PI_SESSION_DIR's creation happens once in
// runRunner instead, never inside engine.NewPi itself.
func buildEngineRegistry(claudeBin, piBin, piModel, piReviewModel, piSessionDir string, piStartupTimeout time.Duration) map[string]engine.Engine {
	return map[string]engine.Engine{
		"claude": engine.NewClaude(claudeBin),
		"pi":     engine.NewPi(piBin, piModel, piReviewModel, piSessionDir, piStartupTimeout),
	}
}

// registeredEngineNames lists every name buildEngineRegistry registers --
// what config.Load validates DEFAULT_ENGINE/REVIEW_ENGINE and repos.json's
// per-repo "engine"/"review_engine" fields against. Derived from
// buildEngineRegistry itself (rather than a separately maintained literal)
// so the two can never drift apart; none of the arguments affect which
// names come out, so every one is "" (or zero) here.
func registeredEngineNames() []string {
	reg := buildEngineRegistry("", "", "", "", "", 0)
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
// unblock, the cause clears on its own.
//
// usageLimit != nil arms/re-arms that task's engine's own auto-resume
// timer regardless of status -- the same hook OnBlocked's ScenarioUsageLimit
// case uses -- since a usage-limit hit pauses that engine whether the task
// itself came out done, blocked, or failed, and that engine must resume on
// its own once its limit resets either way. For done/failed with a non-nil
// usageLimit, the normal message for that status is sent with one extra
// sentence appended naming the engine and noting the pause; the
// blocked+usageLimit message stays exactly as it already was, just naming
// the engine too.
func newTerminalHandler(cfg *config.Config, notifier *notify.Notifier, resumeMgr *resumeTimerMgr) func(job worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *runner.UsageLimitInfo, engineName string) {
	return func(job worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *runner.UsageLimitInfo, engineName string) {
		if usageLimit != nil {
			resumeMgr.handleTrip(engineName, usageLimit.ResetsAt)
		}
		if status == "blocked" && usageLimit != nil {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> Task `%s` (%s) hit the %s usage limit -- %s tasks are **paused** until it resets%s. See its Work Log.",
					cfg.DiscordUserID, job.Slug, job.Repo, engineName, engineName, usageLimit.ResetsAtSuffix()),
				job.Slug+".md", "# Task blocked: usage limit\n\n"+workLog+"\n")
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
			msg := fmt.Sprintf("<@%s> Task `%s` (%s) is **done**: %s -- %s.", cfg.DiscordUserID, job.Slug, job.Repo, prURL, mergeNote)
			if usageLimit != nil {
				msg += usageLimitPauseNote(engineName, usageLimit)
			}
			notifier.Send(job.NotePath, discordThreadID, msg, "", "")
			return
		}
		msg := fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **%s**.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, status)
		if usageLimit != nil {
			msg += usageLimitPauseNote(engineName, usageLimit)
		}
		notifier.Send(job.NotePath, discordThreadID, msg, job.Slug+".md", "# Task "+status+"\n\n"+workLog+"\n")
	}
}

// newMergeGateErrorHandler builds Deps.OnMergeGateError: every non-round-
// limit merge-gate loop error gets the same "PR is still open" message,
// except errors.Is(err, runner.ErrMergedOutsideGate) -- TFO-10's incident,
// where a coding session merged (or closed) the PR itself before the
// gate's review/CI ever ran, so "still open" would be false. That case
// gets its own message instead: the review and CI gate were skipped, and
// a human must check the merged change and its deploy.
func newMergeGateErrorHandler(cfg *config.Config, notifier *notify.Notifier) func(job worker.Job, prURL, discordThreadID string, err error) {
	return func(job worker.Job, prURL, discordThreadID string, err error) {
		if errors.Is(err, runner.ErrMergedOutsideGate) {
			notifier.Send(job.NotePath, discordThreadID,
				fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) PR %s was merged or closed outside the merge gate -- the review and CI gate were skipped, a human must check the merged change and its deploy.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, prURL),
				job.Slug+"-mergegate-error.md", fmt.Sprintf("# Merge-gate loop error\n\n- PR: %s\n- Repo: %s\n- Error: %v\n\nThe PR was merged or closed outside the merge gate -- the review and CI gate were skipped, a human must check the merged change and its deploy.\n", prURL, job.Repo, err))
			return
		}
		notifier.Send(job.NotePath, discordThreadID,
			fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) hit an error in the review/CI merge-gate loop -- PR is still open at %s, needs a human's judgment.", cfg.DiscordUserID, cfg.DiscordAraDevUserID, job.Slug, job.Repo, prURL),
			job.Slug+"-mergegate-error.md", fmt.Sprintf("# Merge-gate loop error\n\n- PR: %s\n- Repo: %s\n- Error: %v\n\nThe review/CI loop ended with an error before reaching a clean merge or exhausting its round budget. The PR is left open; status stays \"done\" in the vault note.\n", prURL, job.Repo, err))
	}
}

// usageLimitPauseNote is the one extra sentence appended to a done/failed
// terminal message when this invocation also hit engineName's usage limit
// -- told apart from the dedicated blocked+usageLimit message above, which
// replaces its wording entirely rather than appending to it.
func usageLimitPauseNote(engineName string, usageLimit *runner.UsageLimitInfo) string {
	return fmt.Sprintf(" The %s usage limit was hit on this run -- %s tasks are **paused** until it resets%s.", engineName, engineName, usageLimit.ResetsAtSuffix())
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
