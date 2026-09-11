package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/disk"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type DiskIOCollector struct {
	every time.Duration

	mu   sync.Mutex
	prev map[string]diskIOStats
}

func NewDiskIO(every time.Duration) *DiskIOCollector {
	return &DiskIOCollector{
		every: every,
		prev:  make(map[string]diskIOStats),
	}
}

func (c *DiskIOCollector) ID() string { return "core.diskio" }

func (c *DiskIOCollector) Every() time.Duration { return c.every }

func (c *DiskIOCollector) Collect(_ context.Context) error {
	var (
		snapshot map[string]diskIOStats
		err      error
	)
	if runtime.GOOS != "linux" {
		snapshot, err = readDiskIOGopsutil()
	} else {
		snapshot, err = readDiskIOProc()
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	dirRead := prostometrics.Label("dir", "read")
	dirWrite := prostometrics.Label("dir", "write")
	for dev, cur := range snapshot {
		deviceLabel := prostometrics.Label("device", dev)

		prev, hadPrev := c.prev[dev]
		c.prev[dev] = cur

		prostometrics.Total("host.disk.io_kb", float64(cur.readBytes)/1024.0,
			deviceLabel, dirRead,
		)

		prostometrics.Total("host.disk.io_kb", float64(cur.writeBytes)/1024.0,
			deviceLabel, dirWrite,
		)

		prostometrics.Total("host.disk.io_ops", float64(cur.readOps),
			deviceLabel, dirRead,
		)

		prostometrics.Total("host.disk.io_ops", float64(cur.writeOps),
			deviceLabel, dirWrite,
		)

		prostometrics.Total("host.disk.io_time_ms", float64(cur.ioTimeMs),
			deviceLabel,
		)

		// How long an average request took, rather than how many there were. Read
		// and write time are combined using their operation counts, so a quiet
		// direction cannot weigh as much as the busy one. One series per device
		// keeps the Host card readable without hiding a slow disk.
		if hadPrev {
			if latency, ok := diskIOLatencyMs(prev, cur); ok {
				prostometrics.Value("host.disk.io_latency_ms", latency, deviceLabel)
			}
		}
	}

	// Forget devices that are gone so an unplugged disk cannot hold memory forever.
	for dev := range c.prev {
		if _, ok := snapshot[dev]; !ok {
			delete(c.prev, dev)
		}
	}

	return nil
}

func diskIOLatencyMs(prev, cur diskIOStats) (float64, bool) {
	ops := diffUint(prev.readOps, cur.readOps) + diffUint(prev.writeOps, cur.writeOps)
	if ops == 0 {
		return 0, false
	}
	elapsed := diffUint(prev.readTimeMs, cur.readTimeMs) + diffUint(prev.writeTimeMs, cur.writeTimeMs)
	return float64(elapsed) / float64(ops), true
}

type diskIOStats struct {
	readBytes   uint64
	writeBytes  uint64
	readOps     uint64
	writeOps    uint64
	readTimeMs  uint64
	writeTimeMs uint64
	ioTimeMs    uint64
}

func readDiskIOProc() (map[string]diskIOStats, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, fmt.Errorf("open /proc/diskstats: %w", err)
	}
	defer f.Close()

	stats := make(map[string]diskIOStats)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) < 14 {
			continue
		}
		dev := fields[2]
		if skipDiskDevice(dev) {
			continue
		}

		readCompleted, _ := parseUint(fields[3])
		sectorsRead, _ := parseUint(fields[5])
		readTimeMs, _ := parseUint(fields[6])
		writeCompleted, _ := parseUint(fields[7])
		sectorsWritten, _ := parseUint(fields[9])
		writeTimeMs, _ := parseUint(fields[10])
		timeInIOms, _ := parseUint(fields[12])

		const sectorSize = 512
		readBytes := sectorsRead * sectorSize
		writeBytes := sectorsWritten * sectorSize

		stats[dev] = diskIOStats{
			readBytes:   readBytes,
			writeBytes:  writeBytes,
			readOps:     readCompleted,
			writeOps:    writeCompleted,
			readTimeMs:  readTimeMs,
			writeTimeMs: writeTimeMs,
			ioTimeMs:    timeInIOms,
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/diskstats: %w", err)
	}
	return stats, nil
}

func readDiskIOGopsutil() (map[string]diskIOStats, error) {
	counters, err := disk.IOCounters()
	if err != nil {
		return nil, fmt.Errorf("disk.IOCounters: %w", err)
	}

	stats := make(map[string]diskIOStats, len(counters))
	for dev, s := range counters {
		if skipDiskDevice(dev) {
			continue
		}

		stats[dev] = diskIOStats{
			readBytes:   s.ReadBytes,
			writeBytes:  s.WriteBytes,
			readOps:     s.ReadCount,
			writeOps:    s.WriteCount,
			readTimeMs:  s.ReadTime,
			writeTimeMs: s.WriteTime,
			ioTimeMs:    s.IoTime,
		}
	}
	return stats, nil
}

func skipDiskDevice(dev string) bool {
	if strings.HasPrefix(dev, "loop") || strings.HasPrefix(dev, "ram") {
		return true
	}
	if strings.HasPrefix(dev, "dm-") {
		return true
	}
	if strings.HasPrefix(dev, "sd") || strings.HasPrefix(dev, "vd") || strings.HasPrefix(dev, "xvd") {
		if hasTrailingDigit(dev) {
			return true
		}
	}
	if strings.HasPrefix(dev, "nvme") && strings.Contains(dev, "p") {
		return true
	}
	return false
}

func hasTrailingDigit(s string) bool {
	if s == "" {
		return false
	}
	last := s[len(s)-1]
	return last >= '0' && last <= '9'
}
