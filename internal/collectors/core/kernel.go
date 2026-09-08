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

// KernelCollector reports the whole-machine counters that explain a disappearance:
// a process the kernel killed for memory, a host thrashing through swap, a script
// forking without end. They are cumulative, so a one-minute cadence loses nothing.
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
		// vmstat counts every fault in pgfault and the major subset in
		// pgmajfault, so reporting pgfault as "minor" would count each major
		// fault on both lines and hide the shift from memory to disk that
		// splitting them is for.
		major, hasMajor := vmstat["pgmajfault"]
		if hasMajor {
			prostometrics.Total("host.page_faults", float64(major), prostometrics.Label("type", "major"))
		}
		if total, ok := vmstat["pgfault"]; ok {
			minor := total
			if hasMajor && total >= major {
				minor = total - major
			}
			prostometrics.Total("host.page_faults", float64(minor), prostometrics.Label("type", "minor"))
		}
		emitSwap := func(dir string, key string) {
			if v, ok := vmstat[key]; ok {
				prostometrics.Total("host.swap_io_pages", float64(v), prostometrics.Label("dir", dir))
			}
		}
		emitSwap("in", "pswpin")
		emitSwap("out", "pswpout")
	}

	stat, statErr := readProcStatCounters()
	if statErr == nil {
		if v, ok := stat["ctxt"]; ok {
			prostometrics.Total("host.context_switches", float64(v))
		}
		if v, ok := stat["intr"]; ok {
			prostometrics.Total("host.interrupts", float64(v))
		}
		if v, ok := stat["processes"]; ok {
			prostometrics.Total("host.forks", float64(v))
		}
	}

	if vmErr != nil {
		return vmErr
	}
	return statErr
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

// readProcStatCounters picks the single-value lines out of /proc/stat and skips
// the per-processor time lines, which cpu_usage already reads.
func readProcStatCounters() (map[string]uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("open /proc/stat: %w", err)
	}
	defer f.Close()

	wanted := map[string]struct{}{
		"ctxt":          {},
		"intr":          {},
		"processes":     {},
		"procs_running": {},
		"procs_blocked": {},
	}
	out := make(map[string]uint64, len(wanted))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if _, ok := wanted[fields[0]]; !ok {
			continue
		}
		// "intr" is followed by a long per-interrupt breakdown; the first value is the total.
		v, err := parseUint(fields[1])
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/stat: %w", err)
	}
	return out, nil
}
