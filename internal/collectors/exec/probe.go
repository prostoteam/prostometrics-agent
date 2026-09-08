package exec

import (
	"context"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

type Probe struct {
	commands []Command
	every    time.Duration
}

func NewProbe(commands []Command, every time.Duration) *Probe {
	copied := make([]Command, len(commands))
	copy(copied, commands)
	return &Probe{commands: copied, every: every}
}

func (p *Probe) ID() string { return "exec" }

func (p *Probe) Detect(_ context.Context) (bool, string) {
	if len(p.commands) == 0 {
		return false, "no commands configured"
	}
	return true, "enabled in config"
}

func (p *Probe) New() agent.Collector { return NewCollector(p.commands, p.every) }
