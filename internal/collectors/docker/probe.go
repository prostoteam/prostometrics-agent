package docker

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

const defaultSockPath = "/var/run/docker.sock"

type Probe struct{}

func NewProbe() *Probe { return &Probe{} }

func (p *Probe) ID() string { return "docker" }

func (p *Probe) Detect(_ context.Context) (bool, string) {
	fi, err := os.Stat(defaultSockPath)
	if err != nil {
		return false, fmt.Sprintf("stat %s: %v", defaultSockPath, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return false, fmt.Sprintf("%s is not a unix socket", defaultSockPath)
	}
	return true, fmt.Sprintf("unix socket detected at %s", defaultSockPath)
}

func (p *Probe) New() agent.Collector {
	c, err := newCPUCollector(defaultSockPath, agent.DockerEvery, "service", 200, 8, 5*time.Second)
	if err != nil {
		return nil
	}
	return c
}
