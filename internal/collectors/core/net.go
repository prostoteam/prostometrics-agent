package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	gnet "github.com/shirou/gopsutil/v3/net"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type NetCollector struct {
	every time.Duration
}

func NewNet(every time.Duration) *NetCollector {
	return &NetCollector{
		every: every,
	}
}

func (c *NetCollector) ID() string { return "core.net" }

func (c *NetCollector) Every() time.Duration { return c.every }

func (c *NetCollector) Collect(_ context.Context) error {
	var (
		snapshot map[string]netCounters
		err      error
	)
	if runtime.GOOS != "linux" {
		snapshot, err = readNetGopsutil()
	} else {
		snapshot, err = readNetProc()
	}
	if err != nil {
		return err
	}

	dirRx := prostometrics.Label("dir", "rx")
	dirTx := prostometrics.Label("dir", "tx")
	for iface, cur := range snapshot {
		ifaceLabel := prostometrics.Label("iface", iface)

		prostometrics.Total("host.net.kb", float64(cur.rxBytes)/1024.0, ifaceLabel, dirRx)
		prostometrics.Total("host.net.packets", float64(cur.rxPackets), ifaceLabel, dirRx)
		prostometrics.Total("host.net.errors", float64(cur.rxErrs), ifaceLabel, dirRx)
		prostometrics.Total("host.net.dropped", float64(cur.rxDrop), ifaceLabel, dirRx)

		prostometrics.Total("host.net.kb", float64(cur.txBytes)/1024.0, ifaceLabel, dirTx)
		prostometrics.Total("host.net.packets", float64(cur.txPackets), ifaceLabel, dirTx)
		prostometrics.Total("host.net.errors", float64(cur.txErrs), ifaceLabel, dirTx)
		prostometrics.Total("host.net.dropped", float64(cur.txDrop), ifaceLabel, dirTx)
	}

	return nil
}

type netCounters struct {
	rxBytes   uint64
	rxPackets uint64
	rxErrs    uint64
	rxDrop    uint64
	txBytes   uint64
	txPackets uint64
	txErrs    uint64
	txDrop    uint64
}

func readNetProc() (map[string]netCounters, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("open /proc/net/dev: %w", err)
	}
	defer f.Close()

	stats := make(map[string]netCounters)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if lineNum <= 2 {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "" {
			continue
		}
		if skipNetInterface(iface) {
			continue
		}

		fields := strings.Fields(strings.TrimSpace(parts[1]))
		if len(fields) < 16 {
			continue
		}

		rxBytes, _ := parseUint(fields[0])
		rxPackets, _ := parseUint(fields[1])
		rxErrs, _ := parseUint(fields[2])
		rxDrop, _ := parseUint(fields[3])

		txBytes, _ := parseUint(fields[8])
		txPackets, _ := parseUint(fields[9])
		txErrs, _ := parseUint(fields[10])
		txDrop, _ := parseUint(fields[11])

		stats[iface] = netCounters{
			rxBytes:   rxBytes,
			rxPackets: rxPackets,
			rxErrs:    rxErrs,
			rxDrop:    rxDrop,
			txBytes:   txBytes,
			txPackets: txPackets,
			txErrs:    txErrs,
			txDrop:    txDrop,
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/net/dev: %w", err)
	}
	return stats, nil
}

func readNetGopsutil() (map[string]netCounters, error) {
	counters, err := gnet.IOCounters(true)
	if err != nil {
		return nil, fmt.Errorf("net.IOCounters: %w", err)
	}

	stats := make(map[string]netCounters, len(counters))
	for _, s := range counters {
		if skipNetInterface(s.Name) {
			continue
		}

		stats[s.Name] = netCounters{
			rxBytes:   s.BytesRecv,
			rxPackets: s.PacketsRecv,
			rxErrs:    s.Errin,
			rxDrop:    s.Dropin,
			txBytes:   s.BytesSent,
			txPackets: s.PacketsSent,
			txErrs:    s.Errout,
			txDrop:    s.Dropout,
		}
	}
	return stats, nil
}

func skipNetInterface(name string) bool {
	// Loopback
	if name == "lo" {
		return true
	}

	// Docker / container veth pairs
	if strings.HasPrefix(name, "veth") {
		return true
	}

	// Docker bridge
	if name == "docker0" {
		return true
	}

	// User / Docker bridge networks like br-xxxxxxxx
	if strings.HasPrefix(name, "br-") {
		return true
	}

	// Common K8s / SDN interfaces
	if strings.HasPrefix(name, "cali") {
		return true
	}
	if strings.HasPrefix(name, "flannel.") {
		return true
	}
	if strings.HasPrefix(name, "tunl") {
		return true
	}

	return false
}
