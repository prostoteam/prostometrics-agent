package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type CPUCollector struct {
	client        *http.Client
	baseURL       string
	labelMode     dockerLabelMode
	labelKey      string
	maxContainers int
	concurrency   int
	timeout       time.Duration
	every         time.Duration

	mu   sync.Mutex
	prev map[string]dockerCPUPrev

	// hostMemTotal is what Docker reports as the machine's memory. A container
	// with no memory limit is handed that number as its limit, so it is the test
	// for "this limit is not really a limit" — without it every unlimited
	// container would appear to be a fixed fraction of the way to being killed.
	hostMemTotal uint64
}

func (c *CPUCollector) ID() string { return "docker.cpu" }

func (c *CPUCollector) Every() time.Duration { return c.every }

func (c *CPUCollector) Collect(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	c.ensureHostMemTotal(ctx)

	// One listing covers both jobs. Asking twice cost an extra request every
	// round and was not atomic: a container that stopped between the two calls
	// was counted as running and then reported no statistics at all.
	all, err := c.listContainers(ctx, c.maxContainers, true)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	containers := c.publishContainerStates(all)
	if len(containers) == 0 {
		c.mu.Lock()
		for k := range c.prev {
			delete(c.prev, k)
		}
		c.mu.Unlock()
		return nil
	}

	active := make(map[string]struct{}, len(containers))
	for _, ctr := range containers {
		active[ctr.ID] = struct{}{}
	}

	workCh := make(chan dockerContainerSummary)
	var wg sync.WaitGroup

	workers := c.concurrency
	if workers > len(containers) {
		workers = len(containers)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctr := range workCh {
				c.collectContainer(ctx, ctr)
			}
		}()
	}

sendLoop:
	for _, ctr := range containers {
		select {
		case <-ctx.Done():
			break sendLoop
		case workCh <- ctr:
		}
	}
	close(workCh)
	wg.Wait()

	c.mu.Lock()
	for id := range c.prev {
		if _, ok := active[id]; !ok {
			delete(c.prev, id)
		}
	}
	c.mu.Unlock()

	return nil
}

// publishContainerStates reports every container the daemon knows about, running
// or not, and returns the running ones for the per-container pass. It is the only
// place a crashed container is visible: the per-service metrics are read from
// live statistics, so a container that stopped simply vanishes from them and its
// chart would otherwise hold the last healthy value it published forever.
// Reporting from the full list means a stopped container keeps reporting zero —
// and a container that was genuinely removed stops reporting altogether, which is
// the difference an alert needs.
func (c *CPUCollector) publishContainerStates(all []dockerContainerSummary) []dockerContainerSummary {
	running := make([]dockerContainerSummary, 0, len(all))
	counts := map[string]int{
		"running":    0,
		"exited":     0,
		"restarting": 0,
		"paused":     0,
		"created":    0,
		"dead":       0,
	}
	for _, ctr := range all {
		state := strings.ToLower(strings.TrimSpace(ctr.State))
		if state == "" {
			continue
		}
		if _, known := counts[state]; !known {
			state = "other"
		}
		counts[state]++

		if state == "running" {
			running = append(running, ctr)
		}

		if label := c.containerLabelValue(ctr); label != "" {
			up := 0.0
			if state == "running" {
				up = 1
			}
			prostometrics.ValueSparse("docker.container.up", up, prostometrics.Label(c.labelKey, label))
		}
	}
	for state, n := range counts {
		prostometrics.ValueSparse("docker.containers_count", float64(n), prostometrics.Label("state", state))
	}
	return running
}

// ensureHostMemTotal keeps trying until the daemon answers. Reading it once and
// giving up would be enough to break memory-limit reporting for the life of the
// process: the agent and the daemon are both boot-time services, so a first
// attempt landing before the daemon is ready is ordinary, and without the host
// total every container looks as though it has a real memory limit.
func (c *CPUCollector) ensureHostMemTotal(ctx context.Context) {
	if c.hostMemTotal > 0 {
		return
	}
	var out dockerInfo
	if err := c.doJSON(ctx, http.MethodGet, "/info", &out); err != nil {
		return
	}
	c.hostMemTotal = out.MemTotal
}

func newCPUCollector(sockPath string, every time.Duration, labelMode string, maxContainers, concurrency int, timeout time.Duration) (*CPUCollector, error) {
	if strings.TrimSpace(sockPath) == "" {
		return nil, errors.New("empty docker socket path")
	}

	mode, key, err := parseDockerLabelMode(labelMode)
	if err != nil {
		return nil, err
	}
	if maxContainers < 1 {
		maxContainers = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 32 {
		concurrency = 32
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if every <= 0 {
		every = 10 * time.Second
	}

	return &CPUCollector{
		client:        dockerUnixClient(sockPath),
		baseURL:       "http://docker",
		labelMode:     mode,
		labelKey:      key,
		maxContainers: maxContainers,
		concurrency:   concurrency,
		timeout:       timeout,
		every:         every,
		prev:          make(map[string]dockerCPUPrev),
	}, nil
}

func parseDockerLabelMode(s string) (dockerLabelMode, string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "service":
		return dockerLabelService, "service", nil
	case "container":
		return dockerLabelContainer, "container", nil
	default:
		return 0, "", fmt.Errorf("invalid docker label mode %q (expected service or container)", s)
	}
}

func (c *CPUCollector) listContainers(ctx context.Context, limit int, all bool) ([]dockerContainerSummary, error) {
	allFlag := 0
	if all {
		allFlag = 1
	}
	p := fmt.Sprintf("/containers/json?all=%d&limit=%d&size=0", allFlag, limit)
	var out []dockerContainerSummary
	if err := c.doJSON(ctx, http.MethodGet, p, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *CPUCollector) collectContainer(ctx context.Context, ctr dockerContainerSummary) {
	if ctr.ID == "" {
		return
	}

	target := c.containerLabelValue(ctr)
	if target == "" {
		return
	}
	targetLabel := prostometrics.Label(c.labelKey, target)

	stats, err := c.getStats(ctx, ctr.ID)
	if err != nil {
		return
	}

	info, hasInfo := c.inspect(ctx, ctr.ID)

	if usage := stats.MemoryStats.Usage; usage > 0 {
		prostometrics.ValueSparse("docker.container.mem.usage_kb", float64(usage)/1024.0,
			targetLabel,
		)
	}

	// A container with no memory limit is handed the host's memory as its limit,
	// which would read as a fixed, meaningless share. Only a real limit is
	// reported, and only then is the share of it worth computing — that share is
	// the number that says how close this container is to being killed.
	if limit := stats.MemoryStats.Limit; limit > 0 && c.isRealMemoryLimit(limit) {
		prostometrics.ValueSparse("docker.container.mem.limit_kb", float64(limit)/1024.0,
			targetLabel,
		)
		if working := stats.MemoryStats.workingSet(); working > 0 {
			pct := 100.0 * float64(working) / float64(limit)
			if pct > 100 {
				pct = 100
			}
			prostometrics.Value("docker.container.mem.limit_used_pct", pct, targetLabel)
		}
	}

	if hasInfo {
		restartCount := info.RestartCount
		if restartCount == 0 {
			restartCount = info.State.RestartCount
		}
		prostometrics.Total("docker.container.restart_count", float64(restartCount),
			targetLabel,
		)

		// A health check that exists and is failing is the earliest signal a
		// service is broken while its process is still alive.
		if info.State.Health != nil {
			healthy := 0.0
			if strings.EqualFold(info.State.Health.Status, "healthy") {
				healthy = 1
			}
			prostometrics.ValueSparse("docker.container.healthy", healthy, targetLabel)
		}

		oomKilled := 0.0
		if info.State.OOMKilled {
			oomKilled = 1
		}
		prostometrics.ValueSparse("docker.container.oom_killed", oomKilled, targetLabel)
	}

	if len(stats.Networks) > 0 {
		var rxBytes uint64
		var txBytes uint64
		for _, net := range stats.Networks {
			rxBytes += net.RxBytes
			txBytes += net.TxBytes
		}
		prostometrics.Total("docker.container.net.kb", float64(rxBytes)/1024.0,
			targetLabel,
			"dir=rx",
		)
		prostometrics.Total("docker.container.net.kb", float64(txBytes)/1024.0,
			targetLabel,
			"dir=tx",
		)
	}

	total := stats.CPUStats.CPUUsage.TotalUsage
	system := stats.CPUStats.SystemCPUUsage
	if total == 0 || system == 0 {
		return
	}

	online := stats.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint32(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if online == 0 {
		online = 1
	}

	throttling := stats.CPUStats.ThrottlingData

	c.mu.Lock()
	prev, ok := c.prev[ctr.ID]
	c.prev[ctr.ID] = dockerCPUPrev{
		totalUsage:       total,
		systemUsage:      system,
		periods:          throttling.Periods,
		throttledPeriods: throttling.ThrottledPeriods,
	}
	c.mu.Unlock()
	if !ok {
		return
	}

	// The share of scheduling periods in which the container was stopped for
	// having used its quota. Zero is the healthy reading and is reported, so a
	// container that starts being throttled shows a line leaving the floor.
	if periods := diffUint(prev.periods, throttling.Periods); periods > 0 {
		throttled := diffUint(prev.throttledPeriods, throttling.ThrottledPeriods)
		pct := 100.0 * float64(throttled) / float64(periods)
		if pct > 100 {
			pct = 100
		}
		prostometrics.Value("docker.container.cpu.throttled_pct", pct, targetLabel)
	}

	cpuDelta := diffUint(prev.totalUsage, total)
	systemDelta := diffUint(prev.systemUsage, system)
	if cpuDelta == 0 || systemDelta == 0 {
		return
	}

	pct := 100.0 * float64(cpuDelta) / float64(systemDelta) * float64(online)
	if pct < 0 || pct != pct {
		return
	}

	prostometrics.Value("docker.container.cpu.usage_pct", pct,
		targetLabel,
	)
}

func (c *CPUCollector) containerLabelValue(ctr dockerContainerSummary) string {
	name := ""
	if len(ctr.Names) > 0 {
		name = strings.TrimPrefix(strings.TrimSpace(ctr.Names[0]), "/")
	}

	if c.labelMode == dockerLabelContainer {
		return name
	}

	if v := strings.TrimSpace(ctr.Labels["com.docker.compose.service"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(ctr.Labels["com.docker.swarm.service.name"]); v != "" {
		return v
	}

	return name
}

func (c *CPUCollector) getStats(ctx context.Context, containerID string) (*dockerStats, error) {
	p := fmt.Sprintf("/containers/%s/stats?stream=false", url.PathEscape(containerID))
	var out dockerStats
	if err := c.doJSON(ctx, http.MethodGet, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *CPUCollector) inspect(ctx context.Context, containerID string) (dockerContainerInfo, bool) {
	p := fmt.Sprintf("/containers/%s/json?size=0", url.PathEscape(containerID))
	var out dockerContainerInfo
	if err := c.doJSON(ctx, http.MethodGet, p, &out); err != nil {
		return dockerContainerInfo{}, false
	}
	return out, true
}

// isRealMemoryLimit rejects the host's own memory, which Docker reports as the
// limit of an unconstrained container. The comparison is approximate because the
// two numbers are read from different places and can differ by a page or two.
func (c *CPUCollector) isRealMemoryLimit(limit uint64) bool {
	if c.hostMemTotal == 0 {
		return true
	}
	return limit < c.hostMemTotal-c.hostMemTotal/100
}

func (c *CPUCollector) doJSON(ctx context.Context, method, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	return dec.Decode(dst)
}

func diffUint(a, b uint64) uint64 {
	if b >= a {
		return b - a
	}
	return 0
}
