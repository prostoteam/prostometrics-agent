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

// NetStatCollector reports network failure rather than network volume. The
// traffic counters already say how much moved; these say how much of it had to be
// sent twice, was refused because a queue was full, or was dropped by the kernel
// before any application saw it — which is what turns "the site feels slow" into
// something with a cause.
//
// Everything here except the socket gauges is cumulative, so the slow cadence
// loses no events.
type NetStatCollector struct {
	every time.Duration
}

func NewNetStat(every time.Duration) *NetStatCollector {
	return &NetStatCollector{every: every}
}

func (c *NetStatCollector) ID() string { return "core.netstat" }

func (c *NetStatCollector) Every() time.Duration { return c.every }

func (c *NetStatCollector) Collect(_ context.Context) error {
	snmp, snmpErr := readProcNetTable("/proc/net/snmp")
	if snmpErr == nil {
		tcp := snmp["Tcp"]
		if v, ok := tcp["RetransSegs"]; ok {
			prostometrics.Total("host.tcp.retransmits", float64(v))
		}
		emitTCPError := func(typ string, key string) {
			if v, ok := tcp[key]; ok {
				prostometrics.Total("host.tcp.errors", float64(v), prostometrics.Label("type", typ))
			}
		}
		emitTCPError("in_errors", "InErrs")
		emitTCPError("attempt_fails", "AttemptFails")
		emitTCPError("estab_resets", "EstabResets")
		emitTCPError("out_resets", "OutRsts")
		if v, ok := tcp["CurrEstab"]; ok {
			prostometrics.ValueSparse("host.tcp.sockets", float64(v), prostometrics.Label("state", "established"))
		}

		udp := snmp["Udp"]
		emitUDPError := func(typ string, key string) {
			if v, ok := udp[key]; ok {
				prostometrics.Total("host.udp.errors", float64(v), prostometrics.Label("type", typ))
			}
		}
		emitUDPError("in_errors", "InErrors")
		emitUDPError("no_ports", "NoPorts")
		emitUDPError("rcvbuf_errors", "RcvbufErrors")
		emitUDPError("sndbuf_errors", "SndbufErrors")
	}

	if netstat, err := readProcNetTable("/proc/net/netstat"); err == nil {
		ext := netstat["TcpExt"]
		emitListenDrop := func(typ string, key string) {
			if v, ok := ext[key]; ok {
				prostometrics.Total("host.tcp.listen_drops", float64(v), prostometrics.Label("type", typ))
			}
		}
		// Overflows are the accept queue being full; drops counts every reason a
		// SYN was discarded. A service that "randomly times out" under load is
		// usually one of these two and nothing else.
		emitListenDrop("overflows", "ListenOverflows")
		emitListenDrop("drops", "ListenDrops")
		if v, ok := ext["TCPSynRetrans"]; ok {
			prostometrics.Total("host.tcp.errors", float64(v), prostometrics.Label("type", "syn_retrans"))
		}
	}

	if sockets, err := readSockStat(); err == nil {
		for state, v := range sockets {
			prostometrics.ValueSparse("host.tcp.sockets", float64(v), prostometrics.Label("state", state))
		}
	}

	// Conntrack exists only when the host filters or forwards traffic. A full
	// table drops packets with no error reported anywhere else.
	if used, err := readUintFromFile("/proc/sys/net/netfilter/nf_conntrack_count"); err == nil {
		prostometrics.ValueSparse("host.conntrack", float64(used), prostometrics.Label("type", "used"))
		if max, err := readUintFromFile("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
			prostometrics.ValueSparse("host.conntrack", float64(max), prostometrics.Label("type", "max"))
		}
	}

	return snmpErr
}

// readProcNetTable parses the paired-line format of /proc/net/snmp and
// /proc/net/netstat: a header line of column names prefixed by a protocol, then a
// value line with the same prefix.
func readProcNetTable(path string) (map[string]map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	out := make(map[string]map[string]uint64, 8)
	var headers []string
	var headerPrefix string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		prefix := strings.TrimSuffix(fields[0], ":")
		if headers == nil || headerPrefix != prefix {
			headers = fields[1:]
			headerPrefix = prefix
			continue
		}

		values := fields[1:]
		row, ok := out[prefix]
		if !ok {
			row = make(map[string]uint64, len(headers))
			out[prefix] = row
		}
		for i, name := range headers {
			if i >= len(values) {
				break
			}
			v, err := parseUint(values[i])
			if err != nil {
				continue
			}
			row[name] = v
		}
		headers = nil
		headerPrefix = ""
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no rows in %s", path)
	}
	return out, nil
}

// readSockStat reports the socket pools that fill up independently of memory:
// sockets waiting out TIME_WAIT, orphaned sockets, and the total allocated.
func readSockStat() (map[string]uint64, error) {
	f, err := os.Open("/proc/net/sockstat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	wanted := map[string]string{
		"tw":     "time_wait",
		"orphan": "orphan",
		"alloc":  "allocated",
		"inuse":  "in_use",
	}
	out := make(map[string]uint64, len(wanted))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || strings.TrimSuffix(fields[0], ":") != "TCP" {
			continue
		}
		for i := 1; i+1 < len(fields); i += 2 {
			label, ok := wanted[fields[i]]
			if !ok {
				continue
			}
			v, err := parseUint(fields[i+1])
			if err != nil {
				continue
			}
			out[label] = v
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/net/sockstat: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no TCP row in /proc/net/sockstat")
	}
	return out, nil
}
