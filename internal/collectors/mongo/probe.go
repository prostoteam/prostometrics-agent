package mongo

import (
	"context"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

type Probe struct {
	instances []Instance
	every     time.Duration
	retry     time.Duration
}

func NewProbe(instances []Instance, every time.Duration, retryEvery time.Duration) *Probe {
	copied := make([]Instance, len(instances))
	copy(copied, instances)
	return &Probe{
		instances: copied,
		every:     every,
		retry:     retryEvery,
	}
}

func (p *Probe) ID() string { return "mongo" }

func (p *Probe) Detect(_ context.Context) (bool, string) {
	if len(p.instances) == 0 {
		return false, "no instances configured"
	}
	return true, "enabled in config"
}

func (p *Probe) New() agent.Collector {
	return NewCollector(p.instances, p.every, p.retry)
}
