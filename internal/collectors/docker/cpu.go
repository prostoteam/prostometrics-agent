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
	// oomSeen is keyed by container ID and guarded by mu, like prev.
	oomSeen map[string]dockerOOMSeen

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
	c.forgetRemovedContainers(all)

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

// publishContainerStates reports every container the daemon knows about that is
// meant to be running, running or not, and returns the running ones for the
// per-container pass. It is the only place a crashed container is visible: the
// per-service metrics are read from live statistics, so a container that stopped
// simply vanishes from them and its chart would otherwise hold the last healthy
// value it published forever. Reporting from the full list means a stopped
// container keeps reporting zero — and a container that was genuinely removed
// stops reporting altogether, which is the difference an alert needs.
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

		// A `docker compose run` container is a one-off command that carries its
		// service's name, so whatever it reported would be mixed into the
		// service's own readings: a shell left open in one fails the service's
		// health check, and one left behind after a failed command reads as the
		// service being partly down. It is counted and nothing more.
		if isOneOff(ctr) {
			continue
		}
		if state == "running" {
			running = append(running, ctr)
		}
		// A container that exited with code 0 finished what it was started for,
		// such as a migration or a setup step; it is the exit Compose's
		// service_completed_successfully waits for. Reported as down, it would hold
		// an alert on for as long as it exists.
		if exitedCleanly(ctr.Status) {
			continue
		}

		// Reported as a success rate rather than a raw flag: the average of a
		// bucket is then the share of the minute the container was up, which
		// reads as a percentage instead of a line pinned to one.
		if label := c.containerLabelValue(ctr); label != "" {
			prostometrics.Success("docker.container.up", state == "running", prostometrics.Label(c.labelKey, label))
		}
	}
	for state, n := range counts {
		prostometrics.ValueSparse("docker.containers_count", float64(n), prostometrics.Label("state", state))
	}
	return running
}

// isOneOff reports whether Compose created the container for a single command.
func isOneOff(ctr dockerContainerSummary) bool {
	return strings.EqualFold(strings.TrimSpace(ctr.Labels["com.docker.compose.oneoff"]), "true")
}

// exitedCleanly reads the exit code from the listing's status, "Exited (0) 3
// hours ago", which is the text `docker ps` prints. The listing carries the code
// nowhere else, and inspecting every stopped container on every round would
// cost a request each. The status is read rather than the state, which Podman
// before 6 reports as "stopped" for the same container. A status in any other
// shape is not a clean exit, so such a container keeps reporting as down.
func exitedCleanly(status string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(status), "Exited (")
	if !ok {
		return false
	}
	code, _, ok := strings.Cut(rest, ")")
	return ok && code == "0"
}

// forgetRemovedContainers drops what was remembered about containers the daemon
// no longer lists. It goes by the full listing rather than by the running set: a
// container waiting for its restart policy is not running, and forgetting it then
// would make its next inspection a first sighting — which never counts as a kill,
// and is exactly when the kill that restarted it shows.
func (c *CPUCollector) forgetRemovedContainers(all []dockerContainerSummary) {
	listed := make(map[string]struct{}, len(all))
	for _, ctr := range all {
		listed[ctr.ID] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.oomSeen {
		if _, ok := listed[id]; !ok {
			delete(c.oomSeen, id)
		}
	}
}

// newOOMKill reports whether the container was killed for memory since it was
// last inspected. Docker's OOMKilled flag is a state that outlives the kill:
// engines before 24 carry it into the run a restart policy starts, and later
// engines set it when any process in the container is killed and clear it only
// when the container next starts. Reported as read, it would hold an alert on for
// as long as the container keeps running.
//
// A kill is therefore the flag turning on, or the flag still on after the
// container started again, which is how a repeated kill shows on an engine that
// carries the flag over. On engines 24 and later the flag then stays set for the
// rest of the run, so a later kill in the same run cannot be told from the first.
// A container inspected for the first time has nothing to compare with, and a
// flag already set then is most likely old: counting it would announce the same
// kill again every time the agent restarts.
func (c *CPUCollector) newOOMKill(containerID string, killed bool, startedAt string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oomSeen == nil {
		c.oomSeen = make(map[string]dockerOOMSeen)
	}
	last, seen := c.oomSeen[containerID]
	c.oomSeen[containerID] = dockerOOMSeen{killed: killed, startedAt: startedAt}
	return seen && killed && (!last.killed || last.startedAt != startedAt)
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
		// service is broken while its process is still alive. Reported as a
		// success rate for the same reason as docker.container.up.
		if info.State.Health != nil {
			prostometrics.Success("docker.container.healthy", strings.EqualFold(info.State.Health.Status, "healthy"), targetLabel)
		}

		oomKilled := 0.0
		if c.newOOMKill(ctr.ID, info.State.OOMKilled, info.State.StartedAt) {
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
