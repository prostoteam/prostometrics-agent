package diagnostics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerDiagnosticsPrioritizeFailuresAndSkipCompletedJobs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	t.Setenv("PM_DIAG_CONTAINER_COMMANDS", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Twelve completed jobs would consume the previous ten-container inspection cap.
	mock := `#!/bin/bash
printf '%s\n' "$*" >> "$PM_DIAG_CONTAINER_COMMANDS"
case "$1" in
 ps)
  case "$*" in
   *--no-trunc*)
    for i in {1..12}; do printf '%064x\texited\tExited (0) 1 hour ago\tfalse\n' "$i"; done
    printf '%064x\texited\tExited (1) 1 hour ago\ttrue\n' 13
    for i in {14..25}; do printf '%064x\trunning\tUp 1 hour (healthy)\tfalse\n' "$i"; done
    printf '%064x\texited\tExited (1) 1 minute ago\tfalse\n' 26
    printf '%064x\trunning\tUp 1 hour (unhealthy)\tfalse\n' 27
    ;;
   *) printf 'host container summary\n';;
  esac;;
 inspect) printf 'container detail\n';;
 logs) printf 'container log\n';;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(mock), 0700); err != nil {
		t.Fatal(err)
	}
	result := collect(context.Background(), Task{Recipe: "containers"})
	if result.ExitCode != 0 {
		t.Fatalf("collection failed: %s", result.Output)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var inspected []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "inspect ") {
			fields := strings.Fields(line)
			inspected = append(inspected, fields[len(fields)-1])
		}
	}
	if len(inspected) != 10 || inspected[0] != fmt.Sprintf("%064x", 27) || inspected[1] != fmt.Sprintf("%064x", 26) {
		t.Fatalf("actual failures were not prioritized: %v", inspected)
	}
	for _, id := range inspected {
		for i := 1; i <= 13; i++ {
			if id == fmt.Sprintf("%064x", i) {
				t.Fatalf("completed/one-off container inspected: %s", id)
			}
		}
	}
}
