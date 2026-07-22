package agent

import "context"

// Probe detects whether an optional integration should be enabled.
// Detection runs once at startup.
type Probe interface {
	ID() string
	Detect(ctx context.Context) (ok bool, reason string)
	New() Collector
}
