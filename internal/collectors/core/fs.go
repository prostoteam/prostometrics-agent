package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/disk"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type FSCollector struct {
	every time.Duration
}

func NewFS(every time.Duration) *FSCollector {
	return &FSCollector{every: every}
}

func (c *FSCollector) ID() string { return "core.fs" }

func (c *FSCollector) Every() time.Duration { return c.every }

func (c *FSCollector) Collect(_ context.Context) error {
	if runtime.GOOS != "linux" {
		return collectFSGopsutil()
	}
	return collectFSProc()
}

func collectFSProc() error {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return fmt.Errorf("open /proc/self/mounts: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mount := fields[1]
		fsType := fields[2]

		if skipFSType(fsType) || skipMountpoint(mount) {
			continue
		}

		emitFSStat(device, mount)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan /proc/self/mounts: %w", err)
	}
	return nil
}

func collectFSGopsutil() error {
	partitions, err := disk.Partitions(true)
	if err != nil {
		return fmt.Errorf("disk.Partitions: %w", err)
	}

	for _, p := range partitions {
		if skipFSType(p.Fstype) || skipMountpoint(p.Mountpoint) {
			continue
		}
		emitFSStat(p.Device, p.Mountpoint)
	}
	return nil
}

func emitFSStat(device, mount string) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return
	}

	totalBytes := uint64(st.Blocks) * uint64(st.Bsize)
	freeBytes := uint64(st.Bfree) * uint64(st.Bsize)
	usedBytes := uint64(0)
	if totalBytes >= freeBytes {
		usedBytes = totalBytes - freeBytes
	}

	totalInodes := uint64(st.Files)
	freeInodes := uint64(st.Ffree)
	usedInodes := uint64(0)
	if totalInodes >= freeInodes {
		usedInodes = totalInodes - freeInodes
	}

	totalKB := totalBytes / 1024
	freeKB := freeBytes / 1024
	usedKB := usedBytes / 1024

	mountLabel := prostometrics.Label("mount", mount)
	deviceLabel := prostometrics.Label("device", device)

	emitSpace := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.fs.capacity_kb", float64(v),
			mountLabel,
			deviceLabel,
			prostometrics.Label("type", typ),
		)
	}
	emitInodes := func(typ string, v uint64) {
		prostometrics.ValueSparse("host.fs.inodes_count", float64(v),
			mountLabel,
			deviceLabel,
			prostometrics.Label("type", typ),
		)
	}

	emitSpace("total", totalKB)
	emitSpace("used", usedKB)
	emitSpace("free", freeKB)

	emitInodes("total", totalInodes)
	emitInodes("used", usedInodes)
	emitInodes("free", freeInodes)
}

func skipFSType(fsType string) bool {
	switch fsType {
	case "proc", "sysfs", "tmpfs", "devtmpfs", "devpts", "cgroup", "cgroup2",
		"overlay", "squashfs", "debugfs", "mqueue", "rpc_pipefs", "nsfs",
		"tracefs", "autofs", "binfmt_misc", "pstore", "fusectl", "configfs":
		return true
	default:
		return false
	}
}

func skipMountpoint(mount string) bool {
	if mount == "/proc" || mount == "/sys" || mount == "/dev" {
		return true
	}
	if strings.HasPrefix(mount, "/run") {
		return true
	}
	return false
}
