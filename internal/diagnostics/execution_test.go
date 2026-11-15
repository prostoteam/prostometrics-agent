package diagnostics

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireGNUTimeout(t *testing.T) {
	t.Helper()
	output, err := exec.Command("timeout", "--version").Output()
	if err != nil || !strings.Contains(string(output), "GNU coreutils") {
		t.Skip("GNU timeout required for process-group regression")
	}
}

func TestLegacyTimeoutChildrenStayInCollectionProcessGroup(t *testing.T) {
	requireGNUTimeout(t)
	dir := t.TempDir()
	pidPath, heartbeat := filepath.Join(dir, "child.pid"), filepath.Join(dir, "heartbeat")
	t.Setenv("PM_DIAG_PID", pidPath)
	t.Setenv("PM_DIAG_HEARTBEAT", heartbeat)
	// This deliberately has the old script's bare timeout, without the v2 preamble.
	script := `timeout 3s bash -c 'echo $$ > "$PM_DIAG_PID"; trap "" TERM; while :; do echo tick >> "$PM_DIAG_HEARTBEAT"; sleep .05; done'`
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result := collectScript(ctx, Task{}, script)
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup also makes a failed regression safe to run against the old runner.
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid != syscall.Getpgrp() {
		defer syscall.Kill(-pgid, syscall.SIGKILL)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || !strings.Contains(result.Output, "time limit") || string(before) != string(after) {
		t.Fatal("command continued running after the collection deadline")
	}
}

func TestTimeoutKillsCommandIgnoringTermination(t *testing.T) {
	requireGNUTimeout(t)
	started := time.Now()
	result := collectScript(context.Background(), Task{}, `timeout .1s bash -c 'trap "" TERM; while :; do sleep .05; done'`)
	if elapsed := time.Since(started); elapsed > 3*time.Second || result.ExitCode == 0 || strings.Contains(result.Output, "time limit") {
		t.Fatalf("per-command timeout did not escalate to KILL: elapsed=%s exit=%d", elapsed, result.ExitCode)
	}
}

func TestCommandTimeoutDoesNotLeaveGrandchildren(t *testing.T) {
	requireGNUTimeout(t)
	dir := t.TempDir()
	pidPath, heartbeat := filepath.Join(dir, "grandchild.pid"), filepath.Join(dir, "heartbeat")
	t.Setenv("PM_DIAG_PID", pidPath)
	t.Setenv("PM_DIAG_HEARTBEAT", heartbeat)
	script := `child() { trap '' TERM; while :; do echo tick >> "$PM_DIAG_HEARTBEAT"; sleep .05; done; }
export -f child
timeout .1s bash -c 'trap "" TERM; child & echo $! > "$PM_DIAG_PID"; wait'`
	result := collectScript(context.Background(), Task{}, script)
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid != syscall.Getpgrp() {
		defer syscall.Kill(-pgid, syscall.SIGKILL)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || string(before) != string(after) {
		t.Fatal("command's grandchild outlived the completed collection")
	}
}
