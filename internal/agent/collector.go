package agent

import (
	"context"
	"time"
)

// Collector produces metrics on a fixed cadence.
// Implementations must emit metrics directly via prostometrics.Value / prostometrics.Count.
type Collector interface {
	ID() string
	Every() time.Duration
	Collect(ctx context.Context) error
}

// CollectorCloser is an optional interface for collectors that need shutdown cleanup.
type CollectorCloser interface {
	Close(ctx context.Context) error
}

// CollectorTimeout is an optional interface for collectors whose work legitimately
// takes longer than the default. Reading a kernel file is instant; running a
// user's shell command or scraping an HTTP endpoint is not, and cutting those off
// at the default would report them as permanently failing.
type CollectorTimeout interface {
	Timeout() time.Duration
}
