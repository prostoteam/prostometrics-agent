package prom

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// sample is one parsed line of the text exposition format.
type sample struct {
	name   string
	labels []string
	value  float64
	kind   string
}

const maxLabelsPerSample = 8

// parseExposition reads the Prometheus text format. Only what the agent can
// forward is kept: histogram buckets are dropped, because one histogram would
// otherwise become dozens of series and cost more than everything else the host
// reports put together. The _sum and _count series that accompany them survive,
// which is enough to recover an average.
func parseExposition(reader io.Reader, allowLabel func(string) bool) ([]sample, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	types := make(map[string]string, 64)
	var out []sample

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[1] == "TYPE" {
				types[fields[2]] = strings.ToLower(fields[3])
			}
			continue
		}

		parsed, ok := parseSampleLine(line, allowLabel)
		if !ok {
			continue
		}
		parsed.kind = types[metricFamily(parsed.name)]
		if parsed.kind == "histogram" && strings.HasSuffix(parsed.name, "_bucket") {
			continue
		}
		out = append(out, parsed)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan exposition: %w", err)
	}
	return out, nil
}

// metricFamily maps a series name back to the family its TYPE line named.
func metricFamily(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func parseSampleLine(line string, allowLabel func(string) bool) (sample, bool) {
	nameEnd := strings.IndexAny(line, "{ \t")
	if nameEnd < 0 {
		return sample{}, false
	}
	name := line[:nameEnd]
	rest := line[nameEnd:]

	var labels []string
	if strings.HasPrefix(rest, "{") {
		close := strings.IndexByte(rest, '}')
		if close < 0 {
			return sample{}, false
		}
		labels = parseLabels(rest[1:close], allowLabel)
		rest = rest[close+1:]
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return sample{}, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return sample{}, false
	}

	return sample{name: name, labels: labels, value: value}, true
}

// parseLabels reads the comma-separated label set. Values are quoted and may
// contain escaped quotes and commas, so the scan tracks quoting rather than
// splitting on separators.
func parseLabels(raw string, allowLabel func(string) bool) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	escaped := false

	flush := func() {
		pair := strings.TrimSpace(current.String())
		current.Reset()
		if pair == "" {
			return
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "workload" {
			return
		}
		if allowLabel != nil && !allowLabel(name) {
			return
		}
		value = unquoteLabelValue(strings.TrimSpace(value))
		if value == "" {
			return
		}
		if len(out) >= maxLabelsPerSample {
			return
		}
		out = append(out, name+"="+value)
	}

	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case escaped:
			current.WriteByte(ch)
			escaped = false
		case ch == '\\' && inQuotes:
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
			current.WriteByte(ch)
		case ch == ',' && !inQuotes:
			flush()
		default:
			current.WriteByte(ch)
		}
	}
	flush()
	return out
}

func unquoteLabelValue(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	// Ingest rejects these outright, and a label value carrying them is a
	// formatting accident rather than something a reader wants to see.
	replacer := strings.NewReplacer("|", "_", "\r", " ", "\n", " ")
	return strings.TrimSpace(replacer.Replace(raw))
}
