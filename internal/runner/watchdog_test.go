package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIdleWatchdogKillsProcessGroup is Phase 8's test gate: a fake claude
// stub prints a valid init line, then sleeps forever, having also spawned
// a background child of its own (simulating a Bash-tool-spawned subshell
// that would otherwise survive as an orphan). Confirms the watchdog fires
// within idleTimeout plus a small buffer, and that *both* the stub and its
// spawned child are actually dead afterward -- not just the top-level PID.
func TestIdleWatchdogKillsProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	stubPidFile := filepath.Join(tmpDir, "stub.pid")
	childPidFile := filepath.Join(tmpDir, "child.pid")
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	script := fmt.Sprintf(`#!/usr/bin/env bash
echo $$ > %q
sleep 600 &
echo $! > %q
echo '{"type":"system","subtype":"init","session_id":"fake-stub-session"}'
sleep 999999
`, stubPidFile, childPidFile)

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake claude stub: %v", err)
	}

	const idleTimeout = 2 * time.Second
	start := time.Now()
	sessionID, err := invokeClaude(scriptPath, tmpDir, "irrelevant prompt", "", idleTimeout, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("invokeClaude error = %v, want ErrIdleTimeout", err)
	}
	if sessionID != "fake-stub-session" {
		t.Errorf("sessionID = %q, want %q (should be captured from the init line before it went idle)", sessionID, "fake-stub-session")
	}

	const buffer = 5 * time.Second
	if elapsed > idleTimeout+buffer {
		t.Errorf("watchdog took %v to fire, want within idleTimeout(%v)+buffer(%v)", elapsed, idleTimeout, buffer)
	}
	if elapsed < idleTimeout {
		t.Errorf("watchdog fired after only %v, before idleTimeout(%v) even elapsed", elapsed, idleTimeout)
	}

	stubPID := readPIDFile(t, stubPidFile)
	childPID := readPIDFile(t, childPidFile)

	// Give the kernel a brief moment to finish reaping, then confirm
	// neither process is still alive.
	deadline := time.Now().Add(3 * time.Second)
	for {
		stubDead := !processAlive(stubPID)
		childDead := !processAlive(childPID)
		if stubDead && childDead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still alive after watchdog fired: stub(pid=%d) alive=%v, child(pid=%d) alive=%v",
				stubPID, processAlive(stubPID), childPID, processAlive(childPID))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading pid file %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing pid from %s (%q): %v", path, data, err)
	}
	return pid
}

func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}
