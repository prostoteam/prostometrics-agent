// Package exec runs user-supplied commands and reports the numbers they print.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	defaultTimeout = 10 * time.Second
	maxOutputBytes = 64 << 10

	// maxLinesPerCommand bounds how many series one command may publish, so a
	// script that starts printing a line per user cannot quietly become the most
	// expensive thing on the host.
	maxLinesPerCommand = 50
)

// Command is one configured shell command.
type Command struct {
	Metric  string
	Path    string
	Args    []string
	Kind    string
	Labels  []string
	Timeout time.Duration
}

// Collector runs each configured command and publishes what it prints. It is the
// escape hatch for everything the agent has no dedicated support for: the age of
// a backup, the size of a directory, a row count, a licence expiry — anything a
// team can already answer with a one-line script.
type Collector struct {
	every    time.Duration
	timeout  time.Duration
	commands []Command
}

func NewCollector(commands []Command, every time.Duration) *Collector {
	// Commands run one after another inside a single collection, so the budget
	// the runner must allow is their sum. Asking for the largest one instead
	// would let the first slow command consume the deadline and leave every
	// command behind it cancelled before it ran.
	var total time.Duration
	for _, command := range commands {
		if command.Timeout > 0 {
			total += command.Timeout
			continue
		}
		total += defaultTimeout
	}
	if total < defaultTimeout {
		total = defaultTimeout
	}
	return &Collector{every: every, timeout: total, commands: commands}
}

func (c *Collector) ID() string { return "exec" }

func (c *Collector) Every() time.Duration { return c.every }

// Timeout asks the runner for room to let the slowest configured command finish.
func (c *Collector) Timeout() time.Duration { return c.timeout }

func (c *Collector) Collect(ctx context.Context) error {
	for _, command := range c.commands {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.runCommand(ctx, command); err != nil {
			log.Printf("exec: %s: %v", command.Metric, err)
		}
	}
	return nil
}

func (c *Collector) runCommand(parent context.Context, command Command) error {
	timeout := command.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%w: %s", err, truncate(detail, 200))
		}
		return err
	}
	if stdout.Len() > maxOutputBytes {
		return fmt.Errorf("output of %d bytes exceeds limit", stdout.Len())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	published := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if published >= maxLinesPerCommand {
			return fmt.Errorf("stopped after %d lines", maxLinesPerCommand)
		}
		value, labels, ok := parseOutputLine(line)
		if !ok {
			continue
		}
		// Ingest rejects negatives, so a script reporting one is a bug in the
		// script rather than something to publish as zero and hide.
		if value < 0 {
			return fmt.Errorf("negative value %g is not reportable", value)
		}
		published++

		all := mergeLabels(command.Labels, labels)
		if strings.EqualFold(command.Kind, "counter") {
			prostometrics.Count(command.Metric, value, all...)
			continue
		}
		prostometrics.ValueSparse(command.Metric, value, all...)
	}
	if published == 0 {
		return fmt.Errorf("no number in output")
	}
	return nil
}

// parseOutputLine reads "<number>" or "<number> key=value key=value". One number
// per line keeps the contract something a shell script can satisfy with echo.
func parseOutputLine(line string) (float64, []string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, nil, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, nil, false
	}

	var labels []string
	for _, field := range fields[1:] {
		name, raw, ok := strings.Cut(field, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || name == "workload" {
			continue
		}
		value := strings.Trim(strings.TrimSpace(raw), `"'`)
		if value == "" || strings.ContainsAny(value, "|\r\n") {
			continue
		}
		labels = append(labels, name+"="+value)
	}
	return value, labels, true
}

// mergeLabels combines the operator's configured labels with the ones the
// command printed, keeping the configured value when both name the same label.
// Ingest rejects an event whose labels repeat a name, and it drops the whole
// sample rather than the offending label, so a script that happens to print a
// label the configuration already sets would otherwise make the metric vanish
// with nothing logged anywhere.
func mergeLabels(configured []string, printed []string) []string {
	if len(printed) == 0 {
		return configured
	}
	merged := make([]string, 0, len(configured)+len(printed))
	seen := make(map[string]struct{}, len(configured)+len(printed))
	for _, pair := range append(append([]string{}, configured...), printed...) {
		name, _, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, pair)
	}
	return merged
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
