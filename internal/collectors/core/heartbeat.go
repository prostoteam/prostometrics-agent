package core

import (
	"context"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// HeartbeatCollector emits one unconditional counter tick. It exists so that a
// host going away is observable at all: every other metric may legitimately fall
// silent — a disk is unmounted, an interface is renamed, a container stops — so
// none of them can distinguish "nothing to report" from "nobody is reporting".
//
// It is a counter with exactly one series and no labels, which is what lets the
// shipped alert read an absent bucket as zero and fire on silence.
type HeartbeatCollector struct {
	every time.Duration
}

func NewHeartbeat(every time.Duration) *HeartbeatCollector {
	return &HeartbeatCollector{every: every}
}

func (c *HeartbeatCollector) ID() string { return "core.heartbeat" }

func (c *HeartbeatCollector) Every() time.Duration { return c.every }

func (c *HeartbeatCollector) Collect(_ context.Context) error {
	prostometrics.Count("host.heartbeat", 1)
	return nil
}
