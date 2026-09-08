package docker

type dockerLabelMode int

const (
	dockerLabelService dockerLabelMode = iota
	dockerLabelContainer
)

type dockerContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
}

type dockerStats struct {
	CPUStats    dockerCPUStats                `json:"cpu_stats"`
	MemoryStats dockerMemoryStats             `json:"memory_stats"`
	Networks    map[string]dockerNetworkStats `json:"networks"`
}

type dockerContainerInfo struct {
	RestartCount uint64 `json:"RestartCount"`
	State        struct {
		RestartCount uint64 `json:"RestartCount"`
		Status       string `json:"Status"`
		OOMKilled    bool   `json:"OOMKilled"`
		ExitCode     int    `json:"ExitCode"`
		Health       *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
}

type dockerInfo struct {
	MemTotal uint64 `json:"MemTotal"`
}

type dockerCPUStats struct {
	CPUUsage       dockerCPUUsage       `json:"cpu_usage"`
	SystemCPUUsage uint64               `json:"system_cpu_usage"`
	OnlineCPUs     uint32               `json:"online_cpus"`
	ThrottlingData dockerThrottlingData `json:"throttling_data"`
}

type dockerCPUUsage struct {
	TotalUsage  uint64   `json:"total_usage"`
	PercpuUsage []uint64 `json:"percpu_usage"`
}

// dockerThrottlingData is how long the scheduler deliberately stopped the
// container because it had used its share. A throttled container is slow while
// the host looks idle, which no other metric here explains.
type dockerThrottlingData struct {
	Periods          uint64 `json:"periods"`
	ThrottledPeriods uint64 `json:"throttled_periods"`
	ThrottledTime    uint64 `json:"throttled_time"`
}

type dockerCPUPrev struct {
	totalUsage       uint64
	systemUsage      uint64
	periods          uint64
	throttledPeriods uint64
}

type dockerMemoryStats struct {
	Usage uint64 `json:"usage"`
	Limit uint64 `json:"limit"`
	// Stats carries the cgroup breakdown. Reported usage includes page cache the
	// kernel would drop under pressure, so the number that matters against the
	// limit is usage minus the reclaimable part.
	Stats map[string]uint64 `json:"stats"`
}

// workingSet returns memory that cannot simply be reclaimed, matching what the
// docker CLI shows. cgroup v2 names the reclaimable part inactive_file; v1 calls
// it cache, and total_inactive_file when hierarchical accounting is on.
func (m dockerMemoryStats) workingSet() uint64 {
	if m.Usage == 0 {
		return 0
	}
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		if reclaimable, ok := m.Stats[key]; ok {
			if m.Usage > reclaimable {
				return m.Usage - reclaimable
			}
			return 0
		}
	}
	return m.Usage
}

type dockerNetworkStats struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}
