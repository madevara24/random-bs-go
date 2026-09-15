// Command pmrunner is the PM task-runner pipeline's single binary: it can
// run as the long-lived "runner" daemon (dispatch/worker/task-processing)
// or as "watcher" (pmwatch), the separate monitoring poller. Same config,
// same binary, mode picked by the first CLI argument.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/madevara24/random-bs-go/internal/config"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pmrunner <runner|watcher> [--config-dir=<dir>]")
}

// resolveConfigDir picks the directory holding .env + repos.json: an
// explicit --config-dir flag, else $PMRUNNER_CONFIG_DIR, else the current
// working directory.
func resolveConfigDir(args []string) string {
	for _, a := range args {
		const prefix = "--config-dir="
		if len(a) > len(prefix) && a[:len(prefix)] == prefix {
			return a[len(prefix):]
		}
	}
	if v := os.Getenv("PMRUNNER_CONFIG_DIR"); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	mode := os.Args[1]
	switch mode {
	case "runner", "watcher":
		// valid, fall through
	case "-h", "--help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "pmrunner: unknown mode %q\n", mode)
		usage()
		os.Exit(2)
	}

	configDir := resolveConfigDir(os.Args[2:])
	envPath := filepath.Join(configDir, ".env")
	reposPath := filepath.Join(configDir, "repos.json")

	cfg, err := config.Load(envPath, reposPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pmrunner: fatal: %v\n", err)
		os.Exit(1)
	}

	switch mode {
	case "runner":
		runRunner(cfg)
	case "watcher":
		runWatcher(cfg)
	}
}
