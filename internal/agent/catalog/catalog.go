package catalog

import (
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/core"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/docker"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/systemd"
)

func CoreCollectors() []agent.Collector {
	return []agent.Collector{
		core.NewMem(agent.CoreFastEvery),
		core.NewNet(agent.CoreFastEvery),
		core.NewDiskIO(agent.CoreFastEvery),
		core.NewCPUUsage(agent.CoreFastEvery),
		core.NewLoadAvg(agent.CoreFastEvery),
		core.NewHeartbeat(agent.CoreFastEvery),
		core.NewUptime(agent.CoreSlowEvery),
		core.NewFS(agent.CoreSlowEvery),
	}
}

// IntegrationProbes covers everything that may be absent on a given host. The
// Linux-only kernel readers are probed like third-party services so an
// unsupported kernel — or a development run on macOS — stays silent instead of
// logging the same missing file forever.
func IntegrationProbes() []agent.Probe {
	return []agent.Probe{
		docker.NewProbe(),
		core.NewPressureProbe(agent.CoreFastEvery),
		core.NewFileProbe("kernel", []string{"/proc/vmstat"}, agent.CoreSlowEvery,
			func(every time.Duration) agent.Collector { return core.NewKernel(every) }),
		core.NewFileProbe("procs", []string{"/proc/sys/fs/file-nr"}, agent.CoreSlowEvery,
			func(every time.Duration) agent.Collector { return core.NewProcs(every) }),
		core.NewFileProbe("netstat", []string{"/proc/net/snmp"}, agent.CoreSlowEvery,
			func(every time.Duration) agent.Collector { return core.NewNetStat(every) }),
		systemd.NewProbe(agent.SystemdEvery),
	}
}
