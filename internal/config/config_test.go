package config

import (
	"os"
	"path/filepath"
	"testing"
)

const baseEnv = "DISCORD_WEBHOOK_URL=https://example.com/webhook\n" +
	"DISCORD_USER_ID=user1\n" +
	"DISCORD_ARA_DEV_USER_ID=user2\n" +
	"GLOBAL_SLOTS=2\n" +
	"IDLE_TIMEOUT_MINUTES=15\n" +
	"VAULT_PATH=/tmp/vault\n" +
	"VAULT_DEFAULT_BRANCH=master\n"

const baseRepos = `{"repo-a": {"path": "/tmp/repo-a", "remote": "git@example.com:a.git", "default_branch": "main"}}`

func writeConfigFiles(t *testing.T, envContent, reposContent string) (envPath, reposPath string) {
	t.Helper()
	dir := t.TempDir()
	envPath = filepath.Join(dir, ".env")
	reposPath = filepath.Join(dir, "repos.json")
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
	if err := os.WriteFile(reposPath, []byte(reposContent), 0o644); err != nil {
		t.Fatalf("writing repos.json: %v", err)
	}
	return envPath, reposPath
}

func TestLoadDefaultsEngineKeysWhenUnset(t *testing.T) {
	envPath, reposPath := writeConfigFiles(t, baseEnv, baseRepos)
	cfg, err := Load(envPath, reposPath, []string{"claude"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultEngine != "claude" {
		t.Errorf("DefaultEngine = %q, want %q", cfg.DefaultEngine, "claude")
	}
	if cfg.ClaudeBin != "claude" {
		t.Errorf("ClaudeBin = %q, want %q", cfg.ClaudeBin, "claude")
	}
	if cfg.ReviewEngine != "claude" {
		t.Errorf("ReviewEngine = %q, want %q", cfg.ReviewEngine, "claude")
	}
	repo := cfg.Repos["repo-a"]
	if repo.Engine != "" || repo.ReviewEngine != "" {
		t.Errorf("repo-a Engine=%q ReviewEngine=%q, want both empty when unset", repo.Engine, repo.ReviewEngine)
	}
}

func TestLoadHonorsEngineKeysWhenSet(t *testing.T) {
	env := baseEnv + "DEFAULT_ENGINE=claude\nCLAUDE_BIN=/opt/claude/bin/claude\nREVIEW_ENGINE=claude\n"
	repos := `{"repo-a": {"path": "/tmp/repo-a", "remote": "git@example.com:a.git", "default_branch": "main", "engine": "claude", "review_engine": "claude"}}`
	envPath, reposPath := writeConfigFiles(t, env, repos)
	cfg, err := Load(envPath, reposPath, []string{"claude"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClaudeBin != "/opt/claude/bin/claude" {
		t.Errorf("ClaudeBin = %q, want the configured path", cfg.ClaudeBin)
	}
	repo := cfg.Repos["repo-a"]
	if repo.Engine != "claude" || repo.ReviewEngine != "claude" {
		t.Errorf("repo-a Engine=%q ReviewEngine=%q, want both %q", repo.Engine, repo.ReviewEngine, "claude")
	}
}

func TestLoadRejectsUnknownDefaultEngine(t *testing.T) {
	env := baseEnv + "DEFAULT_ENGINE=nope\n"
	envPath, reposPath := writeConfigFiles(t, env, baseRepos)
	_, err := Load(envPath, reposPath, []string{"claude"})
	if err == nil {
		t.Fatal("Load: want an error for an unregistered DEFAULT_ENGINE, got nil")
	}
}

func TestLoadRejectsUnknownReviewEngine(t *testing.T) {
	env := baseEnv + "REVIEW_ENGINE=nope\n"
	envPath, reposPath := writeConfigFiles(t, env, baseRepos)
	_, err := Load(envPath, reposPath, []string{"claude"})
	if err == nil {
		t.Fatal("Load: want an error for an unregistered REVIEW_ENGINE, got nil")
	}
}

func TestLoadRejectsUnknownRepoEngine(t *testing.T) {
	repos := `{"repo-a": {"path": "/tmp/repo-a", "remote": "git@example.com:a.git", "default_branch": "main", "engine": "nope"}}`
	envPath, reposPath := writeConfigFiles(t, baseEnv, repos)
	_, err := Load(envPath, reposPath, []string{"claude"})
	if err == nil {
		t.Fatal("Load: want an error for an unregistered repo \"engine\", got nil")
	}
}

func TestLoadRejectsUnknownRepoReviewEngine(t *testing.T) {
	repos := `{"repo-a": {"path": "/tmp/repo-a", "remote": "git@example.com:a.git", "default_branch": "main", "review_engine": "nope"}}`
	envPath, reposPath := writeConfigFiles(t, baseEnv, repos)
	_, err := Load(envPath, reposPath, []string{"claude"})
	if err == nil {
		t.Fatal("Load: want an error for an unregistered repo \"review_engine\", got nil")
	}
}
