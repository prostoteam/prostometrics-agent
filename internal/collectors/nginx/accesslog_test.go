package nginx

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAccessLogLineCombined(t *testing.T) {
	line := `10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET /orders?id=7 HTTP/1.1" 502 1234 "-" "curl/8.0"`
	entry, ok := parseAccessLogLine(line)
	if !ok {
		t.Fatal("expected the stock combined format to parse")
	}
	if entry.statusClass != "5xx" {
		t.Fatalf("statusClass = %q, want 5xx", entry.statusClass)
	}
	if entry.hasRequestTime {
		t.Fatal("the stock format carries no request time, so none should be reported")
	}
}

func TestParseAccessLogLineTimedFormats(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantRequest  float64
		wantUpstream float64
		wantUp       bool
	}{
		{
			name:         "bare decimals appended after the quoted fields",
			line:         `10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 12 "-" "ua" 0.512 0.480`,
			wantRequest:  0.512,
			wantUpstream: 0.480,
			wantUp:       true,
		},
		{
			name:        "named rt= form",
			line:        `10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 12 "-" "ua" rt=0.25 urt="0.20"`,
			wantRequest: 0.25, wantUpstream: 0.20, wantUp: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := parseAccessLogLine(tc.line)
			if !ok || !entry.hasRequestTime {
				t.Fatalf("expected a request time, got ok=%v hasRequestTime=%v", ok, entry.hasRequestTime)
			}
			if entry.requestTimeSec != tc.wantRequest {
				t.Fatalf("requestTimeSec = %v, want %v", entry.requestTimeSec, tc.wantRequest)
			}
			if entry.hasUpstreamTime != tc.wantUp || entry.upstreamTimeSec != tc.wantUpstream {
				t.Fatalf("upstream = %v/%v, want %v/%v",
					entry.hasUpstreamTime, entry.upstreamTimeSec, tc.wantUp, tc.wantUpstream)
			}
		})
	}
}

// The byte count is an integer on every line, and mistaking it for a duration
// would report every request as taking hundreds of seconds.
func TestParseAccessLogLineIgnoresByteCountAsDuration(t *testing.T) {
	line := `10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 4096 "-" "ua"`
	entry, ok := parseAccessLogLine(line)
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if entry.hasRequestTime {
		t.Fatalf("no duration is present, but %v was reported", entry.requestTimeSec)
	}
}

func TestParseAccessLogLineRejectsNonRequests(t *testing.T) {
	for _, line := range []string{"", "not a log line", `1.1.1.1 - - [x] "GET / HTTP/1.1" abc 1`} {
		if _, ok := parseAccessLogLine(line); ok {
			t.Fatalf("expected %q to be rejected", line)
		}
	}
}

// A first open must skip existing history: an agent restart otherwise replays
// every request already in the file as if it had just happened.
func TestLogTailStartsAtTheEndThenFollows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte("old one\nold two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tail := newLogTail(path)
	defer tail.close()

	lines, err := tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected existing content to be skipped, got %v", lines)
	}

	if err := appendTo(path, "fresh one\n"); err != nil {
		t.Fatal(err)
	}
	lines, err = tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "fresh one" {
		t.Fatalf("lines = %v, want [fresh one]", lines)
	}
}

// logrotate renames the open file and creates a new one, so following the path
// has to notice the file identity changed and read the replacement from its start.
func TestLogTailFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tail := newLogTail(path)
	defer tail.close()
	if _, err := tail.readNewLines(); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(path, filepath.Join(dir, "access.log.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after rotation\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "after rotation" {
		t.Fatalf("lines = %v, want [after rotation]", lines)
	}
}

// Truncation in place keeps the same inode, so only the shrinking size reveals it.
func TestLogTailHandlesTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tail := newLogTail(path)
	defer tail.close()
	if _, err := tail.readNewLines(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("restarted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "restarted" {
		t.Fatalf("lines = %v, want [restarted]", lines)
	}
}

// A line nginx has not finished writing must be held back and completed on the
// next read, not reported as a broken half-line.
func TestLogTailJoinsPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tail := newLogTail(path)
	defer tail.close()
	if _, err := tail.readNewLines(); err != nil {
		t.Fatal(err)
	}

	if err := appendTo(path, "half a "); err != nil {
		t.Fatal(err)
	}
	lines, err := tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected the incomplete line to be held, got %v", lines)
	}

	if err := appendTo(path, "line\n"); err != nil {
		t.Fatal(err)
	}
	lines, err = tail.readNewLines()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "half a line" {
		t.Fatalf("lines = %v, want [half a line]", lines)
	}
}

func appendTo(path string, text string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(text)
	return err
}

// The count of offered values belongs to each reservoir. When it was shared
// across both, a log where only some lines carry timings made every replacement
// less likely than it should be, so late requests — including a latency spike at
// the end of a tick — were systematically under-represented.
func TestReservoirCountsOnlyItsOwnOffers(t *testing.T) {
	s := newReservoir(2)
	s.offer(rand.New(rand.NewSource(1)), 1)
	s.offer(rand.New(rand.NewSource(1)), 2)

	if s.offered != 2 || len(s.samples) != 2 {
		t.Fatalf("offered=%d samples=%v, want 2 of each", s.offered, s.samples)
	}

	// A reservoir that never saw a value must stay empty regardless of how many
	// lines the tick parsed.
	other := newReservoir(2)
	if other.offered != 0 || len(other.samples) != 0 {
		t.Fatalf("untouched reservoir = %d/%v, want empty", other.offered, other.samples)
	}
}

// Every value is kept until the reservoir is full, and past that the reservoir
// stays at its limit while still admitting later values.
func TestReservoirStaysAtItsLimitAndKeepsSampling(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	s := newReservoir(3)
	for i := 0; i < 500; i++ {
		s.offer(r, float64(i))
	}

	if len(s.samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(s.samples))
	}
	if s.offered != 500 {
		t.Fatalf("offered = %d, want 500", s.offered)
	}

	var late int
	for _, v := range s.samples {
		if v >= 250 {
			late++
		}
	}
	if late == 0 {
		t.Fatal("no value from the second half survived; later requests are being dropped")
	}
}

// Fewer offers than the limit must all be kept: a quiet tick should report every
// request it saw rather than a sample of them.
func TestReservoirKeepsEverythingBelowTheLimit(t *testing.T) {
	s := newReservoir(20)
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 5; i++ {
		s.offer(r, float64(i))
	}
	if len(s.samples) != 5 {
		t.Fatalf("samples = %v, want all five", s.samples)
	}
}
