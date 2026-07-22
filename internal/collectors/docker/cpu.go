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
}

func (c *CPUCollector) ID() string { return "docker.cpu" }

func (c *CPUCollector) Every() time.Duration { return c.every }

func (c *CPUCollector) Collect(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	containers, err := c.listContainers(ctx, c.maxContainers)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
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

func (c *CPUCollector) listContainers(ctx context.Context, limit int) ([]dockerContainerSummary, error) {
	p := fmt.Sprintf("/containers/json?all=0&limit=%d&size=0", limit)
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

	restartCount, hasRestart := c.getRestartCount(ctx, ctr.ID)

	if usage := stats.MemoryStats.Usage; usage > 0 {
		prostometrics.ValueSparse("docker.container.mem.usage_kb", float64(usage)/1024.0,
			targetLabel,
		)
	}

	if hasRestart {
		prostometrics.Total("docker.container.restart_count", float64(restartCount),
			targetLabel,
		)
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

	c.mu.Lock()
	prev, ok := c.prev[ctr.ID]
	c.prev[ctr.ID] = dockerCPUPrev{totalUsage: total, systemUsage: system}
	c.mu.Unlock()
	if !ok {
		return
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

func (c *CPUCollector) getRestartCount(ctx context.Context, containerID string) (uint64, bool) {
	p := fmt.Sprintf("/containers/%s/json?size=0", url.PathEscape(containerID))
	var out dockerContainerInfo
	if err := c.doJSON(ctx, http.MethodGet, p, &out); err != nil {
		return 0, false
	}
	if out.RestartCount > 0 {
		return out.RestartCount, true
	}
	return out.State.RestartCount, true
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
