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

// TestLoadDefaultsPiKeysWhenUnset covers RBG-36's config defaults: PI_BIN
// defaults to "pi", PI_MODEL/PI_REVIEW_MODEL default to "" (omit
// --model), PI_SESSION_DIR defaults to "pi-sessions" under the config
// directory, PI_STARTUP_TIMEOUT_SECONDS defaults to 90.
func TestLoadDefaultsPiKeysWhenUnset(t *testing.T) {
	envPath, reposPath := writeConfigFiles(t, baseEnv, baseRepos)
	cfg, err := Load(envPath, reposPath, []string{"claude", "pi"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PiBin != "pi" {
		t.Errorf("PiBin = %q, want %q", cfg.PiBin, "pi")
	}
	if cfg.PiModel != "" {
		t.Errorf("PiModel = %q, want empty", cfg.PiModel)
	}
	if cfg.PiReviewModel != "" {
		t.Errorf("PiReviewModel = %q, want empty (defaults to PiModel)", cfg.PiReviewModel)
	}
	wantSessionDir := filepath.Join(filepath.Dir(envPath), "pi-sessions")
	if cfg.PiSessionDir != wantSessionDir {
		t.Errorf("PiSessionDir = %q, want %q", cfg.PiSessionDir, wantSessionDir)
	}
	if cfg.PiStartupTimeoutSeconds != 90 {
		t.Errorf("PiStartupTimeoutSeconds = %d, want 90", cfg.PiStartupTimeoutSeconds)
	}
}

// TestLoadHonorsPiKeysWhenSet confirms every PI_* key overrides its
// default, and PI_REVIEW_MODEL set explicitly diverges from PI_MODEL
// rather than echoing it.
func TestLoadHonorsPiKeysWhenSet(t *testing.T) {
	env := baseEnv + "PI_BIN=/opt/pi/bin/pi\nPI_MODEL=deepseek/deepseek-flash\nPI_REVIEW_MODEL=deepseek/deepseek-v4-pro\nPI_SESSION_DIR=/var/lib/pmrunner/pi-sessions\nPI_STARTUP_TIMEOUT_SECONDS=45\n"
	envPath, reposPath := writeConfigFiles(t, env, baseRepos)
	cfg, err := Load(envPath, reposPath, []string{"claude", "pi"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PiBin != "/opt/pi/bin/pi" {
		t.Errorf("PiBin = %q, want the configured path", cfg.PiBin)
	}
	if cfg.PiModel != "deepseek/deepseek-flash" {
		t.Errorf("PiModel = %q, want %q", cfg.PiModel, "deepseek/deepseek-flash")
	}
	if cfg.PiReviewModel != "deepseek/deepseek-v4-pro" {
		t.Errorf("PiReviewModel = %q, want %q (explicit, not echoing PiModel)", cfg.PiReviewModel, "deepseek/deepseek-v4-pro")
	}
	if cfg.PiSessionDir != "/var/lib/pmrunner/pi-sessions" {
		t.Errorf("PiSessionDir = %q, want the configured path", cfg.PiSessionDir)
	}
	if cfg.PiStartupTimeoutSeconds != 45 {
		t.Errorf("PiStartupTimeoutSeconds = %d, want 45", cfg.PiStartupTimeoutSeconds)
	}
}

// TestLoadPiReviewModelDefaultsToPiModelWhenUnset confirms PI_REVIEW_MODEL
// specifically echoes whatever PI_MODEL resolved to when
// PI_REVIEW_MODEL itself is never set.
func TestLoadPiReviewModelDefaultsToPiModelWhenUnset(t *testing.T) {
	env := baseEnv + "PI_MODEL=deepseek/deepseek-flash\n"
	envPath, reposPath := writeConfigFiles(t, env, baseRepos)
	cfg, err := Load(envPath, reposPath, []string{"claude", "pi"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PiReviewModel != "deepseek/deepseek-flash" {
		t.Errorf("PiReviewModel = %q, want it to default to PiModel %q", cfg.PiReviewModel, "deepseek/deepseek-flash")
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
