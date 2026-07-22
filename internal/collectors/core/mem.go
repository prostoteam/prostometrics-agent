package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/mem"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type MemCollector struct {
	every time.Duration
}

func NewMem(every time.Duration) *MemCollector {
	return &MemCollector{every: every}
}

func (c *MemCollector) ID() string { return "core.mem" }

func (c *MemCollector) Every() time.Duration { return c.every }

func (c *MemCollector) Collect(_ context.Context) error {
	if runtime.GOOS != "linux" {
		return collectMemGopsutil()
	}
	return collectMemProc()
}

func collectMemProc() error {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer f.Close()

	valuesKB := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		valuesKB[key] = val
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan /proc/meminfo: %w", err)
	}

	memTotal := valuesKB["MemTotal"]
	memFree := valuesKB["MemFree"]
	memAvailable := valuesKB["MemAvailable"]
	swapTotal := valuesKB["SwapTotal"]
	swapFree := valuesKB["SwapFree"]

	memUsed := uint64(0)
	if memTotal >= memAvailable {
		memUsed = memTotal - memAvailable
	}
	swapUsed := uint64(0)
	if swapTotal >= swapFree {
		swapUsed = swapTotal - swapFree
	}

	emitMem := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.mem.capacity_kb", float64(v),
			prostometrics.Label("type", typ),
		)
	}
	emitMem("total", memTotal)
	emitMem("used", memUsed)
	emitMem("free", memFree)
	emitMem("available", memAvailable)

	emitSwap := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.swap.capacity_kb", float64(v),
			prostometrics.Label("type", typ),
		)
	}
	emitSwap("total", swapTotal)
	emitSwap("used", swapUsed)
	emitSwap("free", swapFree)

	return nil
}

func collectMemGopsutil() error {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return fmt.Errorf("VirtualMemory: %w", err)
	}
	sm, err := mem.SwapMemory()
	if err != nil {
		return fmt.Errorf("SwapMemory: %w", err)
	}

	memTotalKB := vm.Total / 1024
	memFreeKB := vm.Free / 1024
	memAvailKB := vm.Available / 1024
	memUsedKB := (vm.Total - vm.Available) / 1024

	swapTotalKB := sm.Total / 1024
	swapFreeKB := sm.Free / 1024
	swapUsedKB := sm.Used / 1024

	emitMem := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.mem.capacity_kb", float64(v),
			prostometrics.Label("type", typ),
		)
	}
	emitMem("total", memTotalKB)
	emitMem("used", memUsedKB)
	emitMem("free", memFreeKB)
	emitMem("available", memAvailKB)

	emitSwap := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.swap.capacity_kb", float64(v),
			prostometrics.Label("type", typ),
		)
	}
	emitSwap("total", swapTotalKB)
	emitSwap("used", swapUsedKB)
	emitSwap("free", swapFreeKB)

	return nil
}
