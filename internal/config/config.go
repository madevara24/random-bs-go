// Package config loads the runner/watcher's shared configuration: a .env
// file (Discord credentials, concurrency/timeout knobs, vault location) and
// a repos.json file (per-repo onboarding config). Both run modes read the
// same files. Missing required keys are a fatal, named error — never a
// silent zero-value.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RepoConfig is one entry in repos.json.
type RepoConfig struct {
	Path             string `json:"path"`
	Remote           string `json:"remote"`
	DefaultBranch    string `json:"default_branch"`
	CI               string `json:"ci"`
	AutoMergeDefault bool   `json:"auto_merge_default"`

	// Engine overrides DEFAULT_ENGINE for every task onboarded to this repo
	// that doesn't pin its own `engine:` -- "" means no repo-level
	// override, fall through to DEFAULT_ENGINE.
	Engine string `json:"engine"`

	// ReviewEngine overrides REVIEW_ENGINE for every task onboarded to this
	// repo that doesn't pin its own `review_engine:` -- "" means no
	// repo-level override, fall through to REVIEW_ENGINE.
	ReviewEngine string `json:"review_engine"`
}

// Config is the fully loaded, validated configuration for either run mode.
type Config struct {
	// From .env
	DiscordWebhookURL   string
	DiscordUserID       string
	DiscordAraDevUserID string
	GlobalSlots         int
	IdleTimeoutMinutes  int
	VaultPath           string
	VaultDefaultBranch  string
	HTTPPort            int

	// DiscordTaskForumWebhookURL is optional -- a forum-channel webhook
	// used to give each task its own Discord thread. Empty means the
	// rollout switch is off: every message behaves exactly as before,
	// posted top-level via DiscordWebhookURL.
	DiscordTaskForumWebhookURL string

	// UsageLimitResumeGraceSeconds is how long after rate_limit_info's
	// resetsAt the auto-resume timer waits before actually resuming --
	// resuming exactly at the reset second can race clock drift between
	// this host and Anthropic's, so a grace buffer absorbs that. Optional,
	// defaults to 60.
	UsageLimitResumeGraceSeconds int

	// DefaultEngine is the coding engine a task resolves to when neither
	// its own note's `engine:` nor its repo's "engine" is set. Optional,
	// defaults to "claude".
	DefaultEngine string

	// ClaudeBin is the claude CLI's path/name, passed to engine.NewClaude.
	// Optional, defaults to "claude" (resolved via PATH) -- today's
	// behavior.
	ClaudeBin string

	// ReviewEngine is the engine that reviews a PR in the merge gate when
	// neither the task note's `review_engine:` nor its repo's
	// "review_engine" is set. Resolved independently of DefaultEngine --
	// a task can code with one engine and be reviewed by another. Optional,
	// defaults to "claude".
	ReviewEngine string

	// From repos.json
	Repos map[string]RepoConfig

	// Paths the config was loaded from, kept for diagnostics/logging.
	EnvPath   string
	ReposPath string
}

// requiredEnvKey names a .env key that must be present and non-empty.
type requiredEnvKey struct {
	name string
	// set assigns the parsed value onto cfg. Returns an error naming the key
	// on a parse failure (e.g. non-numeric GLOBAL_SLOTS).
	set func(cfg *Config, raw string) error
}

var requiredKeys = []requiredEnvKey{
	{"DISCORD_WEBHOOK_URL", func(c *Config, v string) error { c.DiscordWebhookURL = v; return nil }},
	{"DISCORD_USER_ID", func(c *Config, v string) error { c.DiscordUserID = v; return nil }},
	{"DISCORD_ARA_DEV_USER_ID", func(c *Config, v string) error { c.DiscordAraDevUserID = v; return nil }},
	{"GLOBAL_SLOTS", func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("GLOBAL_SLOTS must be a positive integer, got %q", v)
		}
		c.GlobalSlots = n
		return nil
	}},
	{"IDLE_TIMEOUT_MINUTES", func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("IDLE_TIMEOUT_MINUTES must be a positive integer, got %q", v)
		}
		c.IdleTimeoutMinutes = n
		return nil
	}},
	{"VAULT_PATH", func(c *Config, v string) error { c.VaultPath = v; return nil }},
	{"VAULT_DEFAULT_BRANCH", func(c *Config, v string) error { c.VaultDefaultBranch = v; return nil }},
}

// HTTP_PORT is optional, with a default -- not load-bearing enough to
// require, unlike the keys above.
const defaultHTTPPort = 8420

// defaultUsageLimitResumeGraceSeconds is USAGE_LIMIT_RESUME_GRACE_SECONDS's
// default when unset.
const defaultUsageLimitResumeGraceSeconds = 60

// defaultEngineName is DEFAULT_ENGINE/REVIEW_ENGINE's shared fallback
// engine name -- the only engine that exists today, so an unset .env keeps
// every task resolving to exactly today's behavior.
const defaultEngineName = "claude"

// defaultClaudeBin is CLAUDE_BIN's default -- the bare command name,
// resolved via PATH, same as every invocation before this config key
// existed.
const defaultClaudeBin = "claude"

// parseEnvFile reads a simple KEY=VALUE .env file: blank lines and lines
// starting with '#' are ignored, no quoting/escaping support (matches the
// bash pipeline's existing .env files).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("%s:%d: malformed line (no '='): %q", path, lineNo, line)
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Strip matching surrounding quotes, if present.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}
	return out, nil
}

// loadRepos reads and validates repos.json.
func loadRepos(path string) (map[string]RepoConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading repos file %s: %w", path, err)
	}
	var repos map[string]RepoConfig
	if err := json.Unmarshal(data, &repos); err != nil {
		return nil, fmt.Errorf("parsing repos file %s: %w", path, err)
	}
	for key, r := range repos {
		if r.Path == "" {
			return nil, fmt.Errorf("repos file %s: repo %q missing required field \"path\"", path, key)
		}
		if r.Remote == "" {
			return nil, fmt.Errorf("repos file %s: repo %q missing required field \"remote\"", path, key)
		}
		if r.DefaultBranch == "" {
			return nil, fmt.Errorf("repos file %s: repo %q missing required field \"default_branch\"", path, key)
		}
	}
	return repos, nil
}

// Load reads and validates both files. envPath and reposPath must be
// explicit -- callers (main.go) resolve them from flags/env/defaults, config
// itself has no opinion on discovery. knownEngines names every engine
// registered in the running binary (see cmd/pmrunner/runner_mode.go's
// registeredEngineNames) -- DEFAULT_ENGINE, REVIEW_ENGINE, and every
// repo's "engine"/"review_engine" must each name one of these, or Load
// fails with a clear error rather than letting an unrecognized engine
// name surface only once a task actually tries to run.
func Load(envPath, reposPath string, knownEngines []string) (*Config, error) {
	raw, err := parseEnvFile(envPath)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		HTTPPort:                     defaultHTTPPort,
		UsageLimitResumeGraceSeconds: defaultUsageLimitResumeGraceSeconds,
		DefaultEngine:                defaultEngineName,
		ClaudeBin:                    defaultClaudeBin,
		ReviewEngine:                 defaultEngineName,
		EnvPath:                      envPath,
		ReposPath:                    reposPath,
	}

	for _, rk := range requiredKeys {
		v, ok := raw[rk.name]
		if !ok || v == "" {
			return nil, fmt.Errorf("config: required key %q missing or empty in %s", rk.name, envPath)
		}
		if err := rk.set(cfg, v); err != nil {
			return nil, fmt.Errorf("config: %w (in %s)", err, envPath)
		}
	}

	if v, ok := raw["HTTP_PORT"]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("config: HTTP_PORT must be a valid port number, got %q (in %s)", v, envPath)
		}
		cfg.HTTPPort = n
	}

	if v, ok := raw["DISCORD_TASK_FORUM_WEBHOOK_URL"]; ok && v != "" {
		cfg.DiscordTaskForumWebhookURL = v
	}

	if v, ok := raw["USAGE_LIMIT_RESUME_GRACE_SECONDS"]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: USAGE_LIMIT_RESUME_GRACE_SECONDS must be a non-negative integer, got %q (in %s)", v, envPath)
		}
		cfg.UsageLimitResumeGraceSeconds = n
	}

	if v, ok := raw["DEFAULT_ENGINE"]; ok && v != "" {
		cfg.DefaultEngine = v
	}
	if v, ok := raw["CLAUDE_BIN"]; ok && v != "" {
		cfg.ClaudeBin = v
	}
	if v, ok := raw["REVIEW_ENGINE"]; ok && v != "" {
		cfg.ReviewEngine = v
	}

	repos, err := loadRepos(reposPath)
	if err != nil {
		return nil, err
	}
	cfg.Repos = repos

	if err := validateEngineNames(cfg, knownEngines); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateEngineNames fails Load with a clear error if DEFAULT_ENGINE,
// REVIEW_ENGINE, or any repo's "engine"/"review_engine" names an engine
// that isn't in knownEngines -- catching a typo at startup instead of
// letting it surface only once a task actually tries to run.
func validateEngineNames(cfg *Config, knownEngines []string) error {
	known := make(map[string]bool, len(knownEngines))
	for _, n := range knownEngines {
		known[n] = true
	}
	list := strings.Join(knownEngines, ", ")

	if !known[cfg.DefaultEngine] {
		return fmt.Errorf("config: DEFAULT_ENGINE %q is not a registered engine (known: %s)", cfg.DefaultEngine, list)
	}
	if !known[cfg.ReviewEngine] {
		return fmt.Errorf("config: REVIEW_ENGINE %q is not a registered engine (known: %s)", cfg.ReviewEngine, list)
	}
	for key, r := range cfg.Repos {
		if r.Engine != "" && !known[r.Engine] {
			return fmt.Errorf("config: repo %q field \"engine\" %q is not a registered engine (known: %s)", key, r.Engine, list)
		}
		if r.ReviewEngine != "" && !known[r.ReviewEngine] {
			return fmt.Errorf("config: repo %q field \"review_engine\" %q is not a registered engine (known: %s)", key, r.ReviewEngine, list)
		}
	}
	return nil
}
