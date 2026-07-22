package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type CPUUsageCollector struct {
	every time.Duration

	mu   sync.Mutex
	prev map[string]cpuTimes
}

func NewCPUUsage(every time.Duration) *CPUUsageCollector {
	return &CPUUsageCollector{
		every: every,
		prev:  make(map[string]cpuTimes),
	}
}

func (c *CPUUsageCollector) ID() string { return "core.cpu_usage" }

func (c *CPUUsageCollector) Every() time.Duration { return c.every }

func (c *CPUUsageCollector) Collect(_ context.Context) error {
	snapshot, err := readCPUTimes()
	if err != nil {
		return fmt.Errorf("read times: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for name, cur := range snapshot {
		if name == "cpu" { // skip aggregate; only emit per-core series
			continue
		}
		prev, ok := c.prev[name]
		c.prev[name] = cur
		if !ok {
			continue
		}

		deltaUser := diffUint(prev.user, cur.user)
		deltaNice := diffUint(prev.nice, cur.nice)
		deltaSystem := diffUint(prev.system, cur.system)
		deltaIdle := diffUint(prev.idle, cur.idle)
		deltaIOWait := diffUint(prev.iowait, cur.iowait)
		deltaIRQ := diffUint(prev.irq, cur.irq)
		deltaSoftIRQ := diffUint(prev.softirq, cur.softirq)
		deltaSteal := diffUint(prev.steal, cur.steal)

		deltaTotal := deltaUser + deltaNice + deltaSystem + deltaIdle +
			deltaIOWait + deltaIRQ + deltaSoftIRQ + deltaSteal
		if deltaTotal == 0 {
			continue
		}

		cpuLabel := prostometrics.Label("cpu", normalizeCPUName(name))

		emit := func(mode string, delta uint64) {
			if delta == 0 {
				return
			}
			pct := 100.0 * float64(delta) / float64(deltaTotal)
			prostometrics.Value("host.cpu.usage_pct", pct,
				cpuLabel, prostometrics.Label("mode", mode),
			)
		}

		emit("user", deltaUser)
		emit("nice", deltaNice)
		emit("system", deltaSystem)
		emit("idle", deltaIdle)
		emit("iowait", deltaIOWait)
		emit("irq", deltaIRQ)
		emit("softirq", deltaSoftIRQ)
		emit("steal", deltaSteal)
	}

	return nil
}

type cpuTimes struct {
	user      uint64
	nice      uint64
	system    uint64
	idle      uint64
	iowait    uint64
	irq       uint64
	softirq   uint64
	steal     uint64
	guest     uint64
	guestNice uint64
}

func normalizeCPUName(raw string) string {
	if raw == "cpu" {
		return "all"
	}
	return strings.TrimPrefix(raw, "cpu")
}

var errNoCPUs = errors.New("no cpu times")
