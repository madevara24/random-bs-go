package main

import (
	"fmt"

	"github.com/madevara24/random-bs-go/internal/config"
)

// runWatcher is the entrypoint for `pmrunner watcher` (pmwatch). Phase 0:
// just prove the mode is selected and config loaded. Phase 12 replaces this
// body with the real polling loop.
func runWatcher(cfg *config.Config) {
	fmt.Printf("pmrunner: mode=watcher vault=%s idle_timeout=%dm http_port=%d\n",
		cfg.VaultPath, cfg.IdleTimeoutMinutes, cfg.HTTPPort)
}
