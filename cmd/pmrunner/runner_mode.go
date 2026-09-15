package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/daemon"
	"github.com/madevara24/random-bs-go/internal/httpapi"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// stubProcessTask stands in for the real runner.ProcessTask until Phase 6.
// Sleeps briefly, logs what it "did." Phase 6 replaces this wiring, not
// this file's boot sequence.
func stubProcessTask(job worker.Job) error {
	fmt.Printf("[runner] (stub) processing task repo=%s slug=%s notePath=%s\n", job.Repo, job.Slug, job.NotePath)
	time.Sleep(200 * time.Millisecond)
	fmt.Printf("[runner] (stub) done with %s\n", job.Slug)
	return nil
}

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
	// ever make (vaultgit.cleanEnv() strips these unconditionally
	// regardless, but this line is what actually answers the "did the
	// hazard survive the new architecture" question empirically, per
	// Design - Runner.md's GIT_DIR section).
	fmt.Printf("[runner] GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE present in daemon env: %+v\n", vaultgit.EnvSnapshot())

	vault := vaultgit.New(cfg.VaultPath, cfg.VaultDefaultBranch)

	globalSlots := worker.NewGlobalSlots(cfg.GlobalSlots)
	workers := worker.Workers{}
	for key := range cfg.Repos {
		workers[key] = worker.New(key, globalSlots, stubProcessTask)
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
