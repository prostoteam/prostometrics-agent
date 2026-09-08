package nginx

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	// DefaultAccessLogPath is where every distribution's package puts it.
	DefaultAccessLogPath = "/var/log/nginx/access.log"

	// maxBytesPerTick bounds catch-up after the agent, or the machine, was
	// stopped. A log that grew by gigabytes meanwhile is skipped to its end
	// rather than parsed in full, because the point is current traffic.
	maxBytesPerTick = 8 << 20

	// defaultTimeSamples is how many response times are reported per tick. Every
	// request would be both enormous and pointless to send: the product computes
	// percentiles from samples, and a uniform sample of this size per tick is
	// enough to place a p95 while keeping a busy site's cost flat instead of
	// proportional to its traffic.
	defaultTimeSamples = 20

	maxLineBytes = 64 << 10
)

// AccessLogCollector turns the access log into request counts by status class and
// a sample of response times. The status page nginx exposes cannot report either:
// it counts connections, not outcomes, which is why a site returning nothing but
// errors looks identical to a healthy one there.
type AccessLogCollector struct {
	every      time.Duration
	path       string
	maxSamples int
	rand       *rand.Rand
	reader     *logTail
}

func NewAccessLogCollector(path string, every time.Duration, maxSamples int) *AccessLogCollector {
	if strings.TrimSpace(path) == "" {
		path = DefaultAccessLogPath
	}
	if maxSamples <= 0 {
		maxSamples = defaultTimeSamples
	}
	return &AccessLogCollector{
		every:      every,
		path:       path,
		maxSamples: maxSamples,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
		reader:     newLogTail(path),
	}
}

func (c *AccessLogCollector) ID() string { return "nginx.accesslog" }

func (c *AccessLogCollector) Every() time.Duration { return c.every }

func (c *AccessLogCollector) Close(_ context.Context) error { return c.reader.close() }

func (c *AccessLogCollector) Collect(_ context.Context) error {
	lines, err := c.reader.readNewLines()
	if err != nil {
		return err
	}

	classCounts := map[string]uint64{}
	requestTimes := newReservoir(c.maxSamples)
	upstreamTimes := newReservoir(c.maxSamples)

	for _, line := range lines {
		entry, ok := parseAccessLogLine(line)
		if !ok {
			continue
		}
		classCounts[entry.statusClass]++
		if entry.hasRequestTime {
			requestTimes.offer(c.rand, entry.requestTimeSec)
		}
		if entry.hasUpstreamTime {
			upstreamTimes.offer(c.rand, entry.upstreamTimeSec)
		}
	}

	// Sent as the increment this tick rather than as a running total. A total
	// would need a first reading to establish a baseline, and both this
	// accumulator and the client's would restart at zero with the agent, so
	// every restart would swallow the requests seen in its first tick.
	for class, delta := range classCounts {
		if delta == 0 {
			continue
		}
		prostometrics.Count("nginx.requests_count", float64(delta),
			prostometrics.Label("class", class),
		)
	}

	for _, sec := range requestTimes.samples {
		prostometrics.Value("nginx.request_time_ms", sec*1000.0)
	}
	for _, sec := range upstreamTimes.samples {
		prostometrics.Value("nginx.upstream_time_ms", sec*1000.0)
	}

	return nil
}

// reservoir keeps a uniform sample of the values offered to it, so a burst of
// slow requests is as likely to be represented as the quiet traffic around it.
//
// The count of offered values belongs to the reservoir rather than to the tick:
// request time and upstream time are filled by different subsets of the log
// lines, and counting lines that offered nothing to this reservoir would make
// every replacement less likely than it should be, biasing the sample towards
// whatever arrived first.
type reservoir struct {
	samples []float64
	limit   int
	offered int
}

func newReservoir(limit int) *reservoir {
	return &reservoir{limit: limit}
}

func (s *reservoir) offer(r *rand.Rand, v float64) {
	s.offered++
	if len(s.samples) < s.limit {
		s.samples = append(s.samples, v)
		return
	}
	if idx := r.Intn(s.offered); idx < s.limit {
		s.samples[idx] = v
	}
}

type accessLogEntry struct {
	statusClass     string
	requestTimeSec  float64
	upstreamTimeSec float64
	hasRequestTime  bool
	hasUpstreamTime bool
}

// parseAccessLogLine reads the combined format every default nginx install
// writes, and picks up response times when the format was extended with them.
// Nginx escapes quotes inside variables as \x22, so an unescaped quote always
// delimits a field and the request can be located without knowing the format.
func parseAccessLogLine(line string) (accessLogEntry, bool) {
	start := strings.IndexByte(line, '"')
	if start < 0 {
		return accessLogEntry{}, false
	}
	end := strings.IndexByte(line[start+1:], '"')
	if end < 0 {
		return accessLogEntry{}, false
	}
	rest := strings.TrimSpace(line[start+1+end+1:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return accessLogEntry{}, false
	}

	status, err := strconv.Atoi(fields[0])
	if err != nil || status < 100 || status > 599 {
		return accessLogEntry{}, false
	}

	entry := accessLogEntry{statusClass: fmt.Sprintf("%dxx", status/100)}

	// Named forms first: a format carrying rt=/urt= says exactly which is which.
	for _, field := range fields {
		name, raw, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.Trim(raw, `"`), 64)
		if err != nil || v < 0 {
			continue
		}
		switch name {
		case "rt", "request_time":
			entry.requestTimeSec, entry.hasRequestTime = v, true
		case "urt", "upstream_response_time", "uht":
			entry.upstreamTimeSec, entry.hasUpstreamTime = v, true
		}
	}
	if entry.hasRequestTime {
		return entry, true
	}

	// Otherwise take bare decimals appended after the quoted fields, which is how
	// $request_time and $upstream_response_time are conventionally added. Integers
	// are ignored on purpose: the byte count is one, and so are most status-like
	// numbers, while a duration is always written with a decimal point.
	tail := rest
	if last := strings.LastIndexByte(rest, '"'); last >= 0 {
		tail = rest[last+1:]
	}
	for _, field := range strings.Fields(tail) {
		if !strings.Contains(field, ".") {
			continue
		}
		v, err := strconv.ParseFloat(field, 64)
		if err != nil || v < 0 {
			continue
		}
		if !entry.hasRequestTime {
			entry.requestTimeSec, entry.hasRequestTime = v, true
			continue
		}
		if !entry.hasUpstreamTime {
			entry.upstreamTimeSec, entry.hasUpstreamTime = v, true
			break
		}
	}

	return entry, true
}

// logTail follows a file across rotation. Identity is the inode rather than the
// path, because logrotate renames the file the agent holds open and creates a new
// one under the old name.
type logTail struct {
	path    string
	file    *os.File
	inode   uint64
	device  uint64
	offset  int64
	partial string
}

func newLogTail(path string) *logTail { return &logTail{path: path} }

func (t *logTail) close() error {
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

func (t *logTail) readNewLines() ([]string, error) {
	if err := t.ensureOpen(); err != nil {
		return nil, err
	}

	info, err := t.file.Stat()
	if err != nil {
		_ = t.close()
		return nil, fmt.Errorf("stat %s: %w", t.path, err)
	}
	size := info.Size()
	if size < t.offset {
		// The file shrank, so it was truncated in place rather than renamed.
		t.offset = 0
		t.partial = ""
	}
	if size == t.offset {
		return nil, nil
	}
	if size-t.offset > maxBytesPerTick {
		t.offset = size - maxBytesPerTick
		t.partial = ""
	}

	if _, err := t.file.Seek(t.offset, io.SeekStart); err != nil {
		_ = t.close()
		return nil, fmt.Errorf("seek %s: %w", t.path, err)
	}

	var lines []string
	reader := bufio.NewReaderSize(t.file, 64<<10)
	read := int64(0)
	for {
		chunk, err := reader.ReadString('\n')
		read += int64(len(chunk))
		if err != nil {
			// A trailing fragment is a line nginx has not finished writing.
			t.partial += chunk
			if len(t.partial) > maxLineBytes {
				t.partial = ""
			}
			break
		}
		line := t.partial + strings.TrimRight(chunk, "\r\n")
		t.partial = ""
		if line != "" {
			lines = append(lines, line)
		}
	}
	t.offset += read

	return lines, nil
}

// ensureOpen opens the log, and reopens it when rotation replaced the file behind
// the path. A first open starts at the end so an agent restart does not replay
// history the product has already been told about.
func (t *logTail) ensureOpen() error {
	if t.file != nil {
		if rotated, err := t.rotated(); err == nil && !rotated {
			return nil
		}
		_ = t.close()
	}

	file, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat %s: %w", t.path, err)
	}

	firstOpen := t.inode == 0
	t.file = file
	t.device, t.inode = fileIdentity(info)
	t.partial = ""
	if firstOpen {
		t.offset = info.Size()
	} else {
		// A rotated file is read from its start: it is the new, nearly empty one.
		t.offset = 0
	}
	return nil
}

func (t *logTail) rotated() (bool, error) {
	info, err := os.Stat(t.path)
	if err != nil {
		return true, err
	}
	device, inode := fileIdentity(info)
	return device != t.device || inode != t.inode, nil
}

func fileIdentity(info os.FileInfo) (uint64, uint64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}
