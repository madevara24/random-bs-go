package main

import (
	"fmt"

	"github.com/madevara24/random-bs-go/internal/config"
)

// runRunner is the entrypoint for `pmrunner runner`. Phase 0: just prove the
// mode is selected and config loaded. Later phases replace this body with
// the real daemon boot sequence (sync, reconciliation, HTTP server, workers).
func runRunner(cfg *config.Config) {
	fmt.Printf("pmrunner: mode=runner vault=%s repos=%d global_slots=%d idle_timeout=%dm http_port=%d\n",
		cfg.VaultPath, len(cfg.Repos), cfg.GlobalSlots, cfg.IdleTimeoutMinutes, cfg.HTTPPort)
}
