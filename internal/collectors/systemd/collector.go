package systemd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// maxNamedFailedUnits bounds how many failing units are named individually. The
// count is always exact; only the per-unit breakdown is capped, because a host
// where a hundred units failed at once has one problem, not a hundred labels.
const maxNamedFailedUnits = 20

// Collector reports the state of systemd units. A service that crashed and stayed
// down is invisible in every other metric here — the host is healthy, the
// processor is idle, and nothing is running.
type Collector struct {
	every      time.Duration
	binary     string
	lastFailed map[string]struct{}
}

func NewCollector(binary string, every time.Duration) *Collector {
	if strings.TrimSpace(binary) == "" {
		binary = defaultBinary
	}
	return &Collector{
		every:      every,
		binary:     binary,
		lastFailed: make(map[string]struct{}),
	}
}

func (c *Collector) ID() string { return "systemd" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Collect(ctx context.Context) error {
	failed, err := c.listUnits(ctx, "failed")
	if err != nil {
		return err
	}
	loaded, loadedErr := c.listUnits(ctx, "loaded")

	prostometrics.ValueSparse("systemd.units_count", float64(len(failed)),
		prostometrics.Label("state", "failed"),
	)
	if loadedErr == nil {
		prostometrics.ValueSparse("systemd.units_count", float64(len(loaded)),
			prostometrics.Label("state", "loaded"),
		)
	}

	current := make(map[string]struct{}, len(failed))
	for i, unit := range failed {
		if i >= maxNamedFailedUnits {
			break
		}
		current[unit] = struct{}{}
		prostometrics.ValueSparse("systemd.unit_failed", 1, prostometrics.Label("unit", unit))
	}
	// A unit that recovered must report a zero rather than fall silent, otherwise
	// its line keeps the last value it had and the dashboard shows it as still
	// broken forever.
	for unit := range c.lastFailed {
		if _, still := current[unit]; !still {
			prostometrics.ValueSparse("systemd.unit_failed", 0, prostometrics.Label("unit", unit))
		}
	}
	c.lastFailed = current

	return loadedErr
}

// listUnits returns the unit names in a given state.
func (c *Collector) listUnits(ctx context.Context, state string) ([]string, error) {
	args := []string{
		"list-units",
		"--type=service",
		"--state=" + state,
		"--no-legend",
		"--plain",
		"--no-pager",
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var units []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" || strings.HasPrefix(name, "●") {
			continue
		}
		units = append(units, name)
	}
	return units, nil
}

func (c *Collector) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("%s %s: %w: %s", c.binary, strings.Join(args, " "), err, detail)
		}
		return "", fmt.Errorf("%s %s: %w", c.binary, strings.Join(args, " "), err)
	}
	return string(bytes.TrimSpace(stdout.Bytes())), nil
}
