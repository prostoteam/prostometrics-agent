package prom

import (
	"context"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

type Probe struct {
	targets []Target
	every   time.Duration
}

func NewProbe(targets []Target, every time.Duration) *Probe {
	copied := make([]Target, len(targets))
	copy(copied, targets)
	return &Probe{targets: copied, every: every}
}

func (p *Probe) ID() string { return "prometheus" }

func (p *Probe) Detect(_ context.Context) (bool, string) {
	if len(p.targets) == 0 {
		return false, "no targets configured"
	}
	return true, "enabled in config"
}

func (p *Probe) New() agent.Collector { return NewCollector(p.targets, p.every) }
