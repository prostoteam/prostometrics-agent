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

// ProcsCollector reports how many processes exist and in what state, plus the two
// ceilings a host can hit without running out of memory or disk: process slots and
// open file descriptors. Both exhaust silently — the machine looks healthy while
// nothing new can start.
type ProcsCollector struct {
	every time.Duration
}

func NewProcs(every time.Duration) *ProcsCollector {
	return &ProcsCollector{every: every}
}

func (c *ProcsCollector) ID() string { return "core.procs" }

func (c *ProcsCollector) Every() time.Duration { return c.every }

// procStateNames maps the single-letter state in /proc/<pid>/stat to a label a
// reader can act on. Anything else is folded into "other".
var procStateNames = map[byte]string{
	'R': "running",
	'S': "sleeping",
	'D': "uninterruptible",
	'Z': "zombie",
	'T': "stopped",
	't': "stopped",
	'I': "idle",
}

func (c *ProcsCollector) Collect(_ context.Context) error {
	counts, total, err := readProcessStates()
	if err != nil {
		return err
	}

	for state, n := range counts {
		prostometrics.ValueSparse("host.procs_count", float64(n), prostometrics.Label("state", state))
	}
	prostometrics.ValueSparse("host.procs_count", float64(total), prostometrics.Label("state", "total"))
	if limit, err := readUintFromFile("/proc/sys/kernel/pid_max"); err == nil {
		prostometrics.ValueSparse("host.procs_count", float64(limit), prostometrics.Label("state", "limit"))
	}

	if used, max, err := readFileDescriptors(); err == nil {
		prostometrics.ValueSparse("host.fd_count", float64(used), prostometrics.Label("type", "used"))
		if isRealFileMax(max) {
			prostometrics.ValueSparse("host.fd_count", float64(max), prostometrics.Label("type", "max"))
		}
	}

	return nil
}

// readProcessStates walks /proc once. Every state that exists is reported, and
// the states that do not are reported as zero, so a queue of uninterruptible
// processes draining back to none is visible as a line returning to the floor
// rather than as a gap.
func readProcessStates() (map[string]int, int, error) {
	dir, err := os.Open("/proc")
	if err != nil {
		return nil, 0, fmt.Errorf("open /proc: %w", err)
	}
	defer dir.Close()

	counts := map[string]int{
		"running":         0,
		"sleeping":        0,
		"uninterruptible": 0,
		"zombie":          0,
		"stopped":         0,
		"idle":            0,
		"other":           0,
	}
	total := 0
	for {
		names, err := dir.Readdirnames(512)
		if len(names) == 0 {
			if err != nil {
				break
			}
			break
		}
		for _, name := range names {
			if !isAllDigits(name) {
				continue
			}
			state, ok := readProcessState("/proc/" + name + "/stat")
			if !ok {
				// A process that exited between listing and reading is routine.
				continue
			}
			total++
			label, known := procStateNames[state]
			if !known {
				label = "other"
			}
			counts[label]++
		}
		if err != nil {
			break
		}
	}
	if total == 0 {
		return nil, 0, fmt.Errorf("no processes read from /proc")
	}
	return counts, total, nil
}

// readProcessState returns the state character. The command name in field two is
// wrapped in parentheses and may itself contain spaces and brackets, so the state
// is taken after the last ')' rather than by splitting on whitespace.
func readProcessState(path string) (byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+2 >= len(data) {
		return 0, false
	}
	return data[end+2], true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// maxRealFileMax is the largest fs.file-max worth treating as a ceiling. Two
// numbers bound it and both sit above this one. The kernel derives its own
// default from memory -- roughly four hundred thousand descriptors per gigabyte
// -- so only a host with terabytes of RAM computes anything near it. The client
// refuses a value sample past about four hundred and thirty million, so a larger
// reading never reaches the server anyway and is dropped with the metric named
// in the agent's log once a minute.
const maxRealFileMax = 400_000_000

// isRealFileMax rejects a file-max that names no ceiling. systemd raises
// fs.file-max to the largest value a signed 64-bit integer holds, so on a
// current host the third field of /proc/sys/fs/file-nr reads as "no limit"
// rather than as a limit. Charting it would put the used count against a line
// nothing can approach, which says less than leaving the line out.
func isRealFileMax(max uint64) bool {
	return max > 0 && max <= maxRealFileMax
}

// readFileDescriptors reads "allocated unused max" from /proc/sys/fs/file-nr.
func readFileDescriptors() (uint64, uint64, error) {
	f, err := os.Open("/proc/sys/fs/file-nr")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, fmt.Errorf("empty /proc/sys/fs/file-nr")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return 0, 0, fmt.Errorf("invalid /proc/sys/fs/file-nr")
	}
	allocated, err := parseUint(fields[0])
	if err != nil {
		return 0, 0, err
	}
	max, err := parseUint(fields[2])
	if err != nil {
		return 0, 0, err
	}
	return allocated, max, nil
}

func readUintFromFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseUint(strings.TrimSpace(string(data)))
}
