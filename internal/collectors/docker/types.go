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
	} `json:"State"`
}

type dockerCPUStats struct {
	CPUUsage       dockerCPUUsage `json:"cpu_usage"`
	SystemCPUUsage uint64         `json:"system_cpu_usage"`
	OnlineCPUs     uint32         `json:"online_cpus"`
}

type dockerCPUUsage struct {
	TotalUsage  uint64   `json:"total_usage"`
	PercpuUsage []uint64 `json:"percpu_usage"`
}

type dockerCPUPrev struct {
	totalUsage  uint64
	systemUsage uint64
}

type dockerMemoryStats struct {
	Usage uint64 `json:"usage"`
}

type dockerNetworkStats struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}
