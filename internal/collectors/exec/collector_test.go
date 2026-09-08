package exec

import (
	"testing"
	"time"
)

func TestParseOutputLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantValue  float64
		wantLabels []string
		wantOK     bool
	}{
		{name: "a bare number", line: "42", wantValue: 42, wantOK: true},
		{name: "a decimal", line: "  3.5  ", wantValue: 3.5, wantOK: true},
		{
			name:       "a number with labels",
			line:       `12 job=nightly target="/var/backups"`,
			wantValue:  12,
			wantLabels: []string{"job=nightly", "target=/var/backups"},
			wantOK:     true,
		},
		{name: "text", line: "not a number", wantOK: false},
		{name: "empty", line: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, labels, ok := parseOutputLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if value != tc.wantValue {
				t.Fatalf("value = %v, want %v", value, tc.wantValue)
			}
			if len(labels) != len(tc.wantLabels) {
				t.Fatalf("labels = %v, want %v", labels, tc.wantLabels)
			}
			for i := range labels {
				if labels[i] != tc.wantLabels[i] {
					t.Fatalf("labels = %v, want %v", labels, tc.wantLabels)
				}
			}
		})
	}
}

// Ingest refuses a label named workload, and it would drop the whole event
// rather than the single label, so the command's output cannot be allowed to set it.
func TestParseOutputLineDropsReservedWorkloadLabel(t *testing.T) {
	_, labels, ok := parseOutputLine("1 workload=elsewhere job=nightly")
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if len(labels) != 1 || labels[0] != "job=nightly" {
		t.Fatalf("labels = %v, want only job=nightly", labels)
	}
}

// Ingest drops the entire event when a label name repeats, not just the
// duplicate label, so a script printing a label the configuration already sets
// would make the metric disappear with nothing logged anywhere.
func TestMergeLabelsKeepsConfiguredValueOnConflict(t *testing.T) {
	merged := mergeLabels([]string{"job=nightly", "host=a"}, []string{"job=weekly", "target=/srv"})

	want := []string{"job=nightly", "host=a", "target=/srv"}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for i := range want {
		if merged[i] != want[i] {
			t.Fatalf("merged = %v, want %v", merged, want)
		}
	}
}

func TestMergeLabelsWithoutPrintedLabels(t *testing.T) {
	configured := []string{"job=nightly"}
	if got := mergeLabels(configured, nil); len(got) != 1 || got[0] != "job=nightly" {
		t.Fatalf("merged = %v", got)
	}
}

// Commands run one after another inside a single collection, so the deadline the
// runner is asked for has to cover all of them. Asking for the largest single
// timeout let the first slow command spend the budget and left the rest
// cancelled before they ran.
func TestCollectorTimeoutCoversEveryCommand(t *testing.T) {
	c := NewCollector([]Command{
		{Metric: "a", Timeout: 10 * time.Second},
		{Metric: "b", Timeout: 10 * time.Second},
		{Metric: "c", Timeout: 5 * time.Second},
	}, time.Minute)

	if got := c.Timeout(); got != 25*time.Second {
		t.Fatalf("Timeout = %s, want 25s (the sum, not the largest)", got)
	}
}

func TestCollectorTimeoutUsesTheDefaultPerCommand(t *testing.T) {
	c := NewCollector([]Command{{Metric: "a"}, {Metric: "b"}}, time.Minute)
	if got := c.Timeout(); got != 2*defaultTimeout {
		t.Fatalf("Timeout = %s, want %s", got, 2*defaultTimeout)
	}

	empty := NewCollector(nil, time.Minute)
	if got := empty.Timeout(); got != defaultTimeout {
		t.Fatalf("Timeout = %s, want %s", got, defaultTimeout)
	}
}
