package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// KernelCollector reports the whole-machine counters that explain a
// disappearance: a process the kernel killed for memory, a host thrashing
// through swap. They are cumulative, so a one-minute cadence loses nothing.
//
// The scheduler's own counters — context switches, interrupts, processes
// forked — used to be reported here as well and were dropped: they describe how
// the kernel spends itself rather than what happened to the host, and nobody
// running a small fleet acts on them.
type KernelCollector struct {
	every time.Duration
}

func NewKernel(every time.Duration) *KernelCollector {
	return &KernelCollector{every: every}
}

func (c *KernelCollector) ID() string { return "core.kernel" }

func (c *KernelCollector) Every() time.Duration { return c.every }

func (c *KernelCollector) Collect(_ context.Context) error {
	vmstat, vmErr := readKeyedProcFile("/proc/vmstat")
	if vmErr == nil {
		if v, ok := vmstat["oom_kill"]; ok {
			prostometrics.Total("host.oom_kills", float64(v))
		}
		emitSwap := func(dir string, key string) {
			if v, ok := vmstat[key]; ok {
				prostometrics.Total("host.swap_io_pages", float64(v), prostometrics.Label("dir", dir))
			}
		}
		emitSwap("in", "pswpin")
		emitSwap("out", "pswpout")
	}

	return vmErr
}

// readKeyedProcFile parses the "name value" shape used by /proc/vmstat.
func readKeyedProcFile(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	out := make(map[string]uint64, 256)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := parseUint(fields[1])
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}
