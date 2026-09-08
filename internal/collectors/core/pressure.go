package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
	prostometrics "github.com/prostoteam/prostometrics-go"
)

const pressureDir = "/proc/pressure"

// pressureResources are the three kinds of stall the kernel tracks. "some" is the
// share of time at least one task was blocked on the resource, "full" the share
// where every task was — the difference between a machine that is contended and
// one that is stopped.
var pressureResources = []string{"cpu", "memory", "io"}

// PressureCollector reports Linux PSI. It is the closest thing the kernel has to
// a direct answer to "is this machine struggling", because it measures waiting
// rather than utilisation: a disk can be 100% busy without anything waiting on
// it, and a machine can be half idle while every request stalls.
type PressureCollector struct {
	every time.Duration
}

func NewPressure(every time.Duration) *PressureCollector {
	return &PressureCollector{every: every}
}

func (c *PressureCollector) ID() string { return "core.pressure" }

func (c *PressureCollector) Every() time.Duration { return c.every }

func (c *PressureCollector) Collect(_ context.Context) error {
	var firstErr error
	for _, resource := range pressureResources {
		samples, err := readPressureFile(filepath.Join(pressureDir, resource))
		if err != nil {
			// A kernel exposing only part of PSI is normal; keep the rest flowing.
			if firstErr == nil && !os.IsNotExist(err) {
				firstErr = err
			}
			continue
		}
		resourceLabel := prostometrics.Label("resource", resource)
		for kind, avg10 := range samples {
			if avg10 < 0 {
				continue
			}
			prostometrics.Value("host.pressure_pct", avg10,
				resourceLabel,
				prostometrics.Label("kind", kind),
			)
		}
	}
	return firstErr
}

// readPressureFile returns avg10 per stall kind ("some", "full").
func readPressureFile(path string) (map[string]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]float64, 2)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		for _, field := range fields[1:] {
			name, raw, ok := strings.Cut(field, "=")
			if !ok || name != "avg10" {
				continue
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			out[kind] = v
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pressure averages in %s", path)
	}
	return out, nil
}

// PressureProbe keeps the collector off kernels without PSI, so an unsupported
// host stays silent instead of logging a failure every minute forever.
type PressureProbe struct {
	every time.Duration
}

func NewPressureProbe(every time.Duration) *PressureProbe { return &PressureProbe{every: every} }

func (p *PressureProbe) ID() string { return "pressure" }

func (p *PressureProbe) Detect(_ context.Context) (bool, string) {
	for _, resource := range pressureResources {
		path := filepath.Join(pressureDir, resource)
		if _, err := readPressureFile(path); err == nil {
			return true, fmt.Sprintf("PSI available at %s", pressureDir)
		}
	}
	return false, fmt.Sprintf("no readable PSI files in %s (needs Linux 4.20+ with CONFIG_PSI)", pressureDir)
}

func (p *PressureProbe) New() agent.Collector { return NewPressure(p.every) }
