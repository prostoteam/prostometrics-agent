package core

import "testing"

func TestDiskIOLatencyWeightsReadAndWriteByOperationCount(t *testing.T) {
	prev := diskIOStats{readOps: 10, writeOps: 20, readTimeMs: 100, writeTimeMs: 200}
	cur := diskIOStats{readOps: 11, writeOps: 29, readTimeMs: 200, writeTimeMs: 290}

	latency, ok := diskIOLatencyMs(prev, cur)
	if !ok {
		t.Fatal("expected latency for completed operations")
	}
	if latency != 19 {
		t.Fatalf("latency = %v, want 19", latency)
	}
}

func TestDiskIOLatencySkipsIdleInterval(t *testing.T) {
	stats := diskIOStats{readOps: 10, writeOps: 20, readTimeMs: 100, writeTimeMs: 200}
	if _, ok := diskIOLatencyMs(stats, stats); ok {
		t.Fatal("idle interval must not report latency")
	}
}
