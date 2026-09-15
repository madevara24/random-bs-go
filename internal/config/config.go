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
// itself has no opinion on discovery.
func Load(envPath, reposPath string) (*Config, error) {
	raw, err := parseEnvFile(envPath)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		HTTPPort:  defaultHTTPPort,
		EnvPath:   envPath,
		ReposPath: reposPath,
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

	repos, err := loadRepos(reposPath)
	if err != nil {
		return nil, err
	}
	cfg.Repos = repos

	return cfg, nil
}
