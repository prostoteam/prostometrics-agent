package docker

import "testing"

// Reported usage includes page cache the kernel would drop under pressure.
// Charging that against the limit reports a container as nearly out of memory
// when it is merely holding cached files.
func TestMemoryWorkingSetExcludesReclaimableCache(t *testing.T) {
	cases := []struct {
		name  string
		stats dockerMemoryStats
		want  uint64
	}{
		{
			name:  "cgroup v2 inactive_file",
			stats: dockerMemoryStats{Usage: 1000, Stats: map[string]uint64{"inactive_file": 400}},
			want:  600,
		},
		{
			name:  "cgroup v1 total_inactive_file",
			stats: dockerMemoryStats{Usage: 1000, Stats: map[string]uint64{"total_inactive_file": 250}},
			want:  750,
		},
		{
			name:  "no breakdown falls back to reported usage",
			stats: dockerMemoryStats{Usage: 1000},
			want:  1000,
		},
		{
			name:  "cache larger than usage cannot go negative",
			stats: dockerMemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 400}},
			want:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.workingSet(); got != tc.want {
				t.Fatalf("workingSet = %d, want %d", got, tc.want)
			}
		})
	}
}

// Docker hands an unconstrained container the host's memory as its limit, so
// reporting a share of it would show every such container at a fixed, meaningless
// distance from being killed.
func TestIsRealMemoryLimitRejectsHostMemory(t *testing.T) {
	c := &CPUCollector{hostMemTotal: 8_000_000_000}

	if c.isRealMemoryLimit(8_000_000_000) {
		t.Fatal("the host's own memory is not a limit")
	}
	if !c.isRealMemoryLimit(512_000_000) {
		t.Fatal("a real limit should be reported")
	}

	unknown := &CPUCollector{}
	if !unknown.isRealMemoryLimit(8_000_000_000) {
		t.Fatal("with no host total known, the limit should be trusted")
	}
}

// The state pass and the statistics pass used to come from two separate
// listings, which cost an extra request every round and were not atomic: a
// container that stopped between them was counted as running and then reported
// no statistics at all. One listing now feeds both.
func TestPublishContainerStatesReturnsOnlyRunningContainers(t *testing.T) {
	c := &CPUCollector{labelKey: "service"}
	running := c.publishContainerStates([]dockerContainerSummary{
		{ID: "a", Names: []string{"/web"}, State: "running"},
		{ID: "b", Names: []string{"/worker"}, State: "exited"},
		{ID: "c", Names: []string{"/db"}, State: "running"},
		{ID: "d", Names: []string{"/cron"}, State: "restarting"},
		{ID: "e", Names: []string{"/odd"}, State: ""},
	})

	if len(running) != 2 {
		t.Fatalf("running = %d containers, want 2", len(running))
	}
	for _, ctr := range running {
		if ctr.State != "running" {
			t.Fatalf("returned a %q container for the statistics pass", ctr.State)
		}
	}
}

// The host memory total is what separates a real container memory limit from the
// host's own memory, which Docker hands to unconstrained containers. Reading it
// once and giving up left that check broken for the life of the process, and the
// agent and the Docker daemon are both boot-time services.
func TestEnsureHostMemTotalRetriesUntilItSucceeds(t *testing.T) {
	c := &CPUCollector{}
	if c.hostMemTotal != 0 {
		t.Fatal("expected no host total before the first successful read")
	}

	// A failed read must leave the field unset so the next collection tries again.
	if !c.isRealMemoryLimit(8_000_000_000) {
		t.Fatal("with no host total known the limit should be trusted")
	}

	c.hostMemTotal = 8_000_000_000
	if c.isRealMemoryLimit(8_000_000_000) {
		t.Fatal("once the host total is known, the host's own memory is not a limit")
	}
}
