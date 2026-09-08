package prom

import (
	"strings"
	"testing"
	"time"
)

const exposition = `# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{code="200",method="get"} 1027
http_requests_total{code="500",method="get"} 3
# TYPE queue_depth gauge
queue_depth 42
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.1"} 100
request_duration_seconds_bucket{le="+Inf"} 140
request_duration_seconds_sum 53.3
request_duration_seconds_count 140
untyped_metric 7
`

func TestParseExpositionReadsTypesAndLabels(t *testing.T) {
	samples, err := parseExposition(strings.NewReader(exposition), nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string][]sample{}
	for _, s := range samples {
		byName[s.name] = append(byName[s.name], s)
	}

	requests := byName["http_requests_total"]
	if len(requests) != 2 {
		t.Fatalf("http_requests_total series = %d, want 2", len(requests))
	}
	if requests[0].kind != "counter" {
		t.Fatalf("kind = %q, want counter", requests[0].kind)
	}
	if got := strings.Join(requests[0].labels, ","); got != "code=200,method=get" {
		t.Fatalf("labels = %q", got)
	}
	if requests[0].value != 1027 {
		t.Fatalf("value = %v, want 1027", requests[0].value)
	}

	if len(byName["queue_depth"]) != 1 || byName["queue_depth"][0].kind != "gauge" {
		t.Fatalf("queue_depth not read as a gauge: %+v", byName["queue_depth"])
	}
	if byName["untyped_metric"][0].value != 7 {
		t.Fatal("a metric with no TYPE line should still be read")
	}
}

// One histogram is dozens of bucket series. Forwarding them would cost more than
// every host metric put together, while _sum and _count still give an average.
func TestParseExpositionDropsHistogramBuckets(t *testing.T) {
	samples, err := parseExposition(strings.NewReader(exposition), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if strings.HasSuffix(s.name, "_bucket") {
			t.Fatalf("bucket series %q should have been dropped", s.name)
		}
	}

	var sawSum, sawCount bool
	for _, s := range samples {
		switch s.name {
		case "request_duration_seconds_sum":
			sawSum = true
		case "request_duration_seconds_count":
			sawCount = true
		}
	}
	if !sawSum || !sawCount {
		t.Fatalf("sum/count kept = %v/%v, want both", sawSum, sawCount)
	}
}

func TestParseExpositionAppliesLabelAllowlist(t *testing.T) {
	allow := func(name string) bool { return name == "code" }
	samples, err := parseExposition(strings.NewReader(exposition), allow)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		for _, label := range s.labels {
			if !strings.HasPrefix(label, "code=") {
				t.Fatalf("label %q survived an allowlist that only permits code", label)
			}
		}
	}
}

// A label value may contain the characters used to delimit the label set, so the
// scan has to respect quoting rather than split on commas.
func TestParseLabelsHandlesQuotedSeparators(t *testing.T) {
	labels := parseLabels(`path="/a,b",method="get"`, nil)
	if len(labels) != 2 {
		t.Fatalf("labels = %v, want two", labels)
	}
	if labels[0] != "path=/a,b" {
		t.Fatalf("labels[0] = %q, want path=/a,b", labels[0])
	}
}

// Ingest rejects a label named workload outright, which would silently drop the
// whole event rather than just that label.
func TestParseLabelsDropsReservedWorkloadLabel(t *testing.T) {
	labels := parseLabels(`workload="other",code="200"`, nil)
	if len(labels) != 1 || labels[0] != "code=200" {
		t.Fatalf("labels = %v, want only code=200", labels)
	}
}

func TestMatcherSupportsPrefixPatterns(t *testing.T) {
	match := matcher([]string{"traefik_*", "queue_depth"})
	for _, name := range []string{"traefik_service_requests_total", "queue_depth"} {
		if !match(name) {
			t.Fatalf("%q should match", name)
		}
	}
	if match("go_goroutines") {
		t.Fatal("go_goroutines should not match")
	}

	if !matcher(nil)("anything") {
		t.Fatal("an empty allowlist should allow everything")
	}
}

// Targets are scraped one after another. With the default collection deadline
// the first slow endpoint spent the whole budget and every target behind it went
// unscraped in every round, reporting nothing rather than reporting late.
func TestCollectorTimeoutScalesWithTargetCount(t *testing.T) {
	three := NewCollector([]Target{{Name: "a"}, {Name: "b"}, {Name: "c"}}, 30*time.Second)
	if got := three.Timeout(); got != 3*defaultTimeout {
		t.Fatalf("Timeout = %s, want %s", got, 3*defaultTimeout)
	}

	none := NewCollector(nil, 30*time.Second)
	if got := none.Timeout(); got != defaultTimeout {
		t.Fatalf("Timeout = %s, want %s", got, defaultTimeout)
	}
}

func TestNewCollectorAppliesTheDefaultSeriesCap(t *testing.T) {
	c := NewCollector([]Target{{Name: "a"}, {Name: "b", MaxSeries: 5}}, 30*time.Second)
	if c.targets[0].MaxSeries != DefaultMaxSeries {
		t.Fatalf("default MaxSeries = %d, want %d", c.targets[0].MaxSeries, DefaultMaxSeries)
	}
	if c.targets[1].MaxSeries != 5 {
		t.Fatalf("configured MaxSeries = %d, want 5", c.targets[1].MaxSeries)
	}
}
