package catalog

import (
	"github.com/prostoteam/prostometrics-agent/internal/agent"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/core"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/docker"
)

func CoreCollectors() []agent.Collector {
	return []agent.Collector{
		core.NewMem(agent.CoreFastEvery),
		core.NewNet(agent.CoreFastEvery),
		core.NewDiskIO(agent.CoreFastEvery),
		core.NewCPUUsage(agent.CoreFastEvery),
		core.NewUptime(agent.CoreSlowEvery),
		core.NewFS(agent.CoreSlowEvery),
	}
}

func IntegrationProbes() []agent.Probe {
	return []agent.Probe{
		docker.NewProbe(),
	}
}
