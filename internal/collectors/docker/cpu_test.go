package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// fakeDaemon answers the Docker API calls the collector makes. A test changes the
// listing and the inspection results between rounds to walk a container through
// its life.
type fakeDaemon struct {
	mu      sync.Mutex
	listing []dockerContainerSummary
	state   map[string]fakeContainerState
}

type fakeContainerState struct {
	OOMKilled bool
	StartedAt string
	Health    *fakeHealth `json:",omitempty"`
}

type fakeHealth struct {
	Status string
}

func (d *fakeDaemon) set(listing []dockerContainerSummary, state map[string]fakeContainerState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listing = listing
	d.state = state
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var body any
	switch path := r.URL.Path; {
	case path == "/info":
		body = map[string]any{"MemTotal": uint64(8_000_000_000)}
	case path == "/containers/json":
		body = d.listing
	case strings.HasSuffix(path, "/stats"):
		body = map[string]any{"memory_stats": map[string]any{"usage": 4096}}
	case strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		body = map[string]any{"State": d.state[id]}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func newFakeDaemonCollector(t *testing.T, daemon *fakeDaemon) *CPUCollector {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)

	c, err := newCPUCollector("/var/run/docker.sock", time.Second, "service", 200, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.client = server.Client()
	c.baseURL = server.URL
	return c
}

// valueCapture keeps the value readings a flush would have sent.
type valueCapture struct {
	mu     sync.Mutex
	values []prostometrics.ValueEvent
}

func (v *valueCapture) Send(_ context.Context, payload *prostometrics.Payload) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values = append(v.values, payload.Values...)
	return nil
}

// sentReadings is what one collection sent: readings by metric, then by label
// set, in the order they were taken.
type sentReadings map[string]map[string][]float64

// collectRound runs one collection and returns what it sent.
func collectRound(t *testing.T, c *CPUCollector) sentReadings {
	t.Helper()
	capture := &valueCapture{}
	client, err := prostometrics.Init("test", prostometrics.Config{Transport: capture, Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	readings := sentReadings{}
	for _, event := range capture.values {
		if readings[event.Metric] == nil {
			readings[event.Metric] = map[string][]float64{}
		}
		key := strings.Join(event.Labels, ",")
		readings[event.Metric][key] = append(readings[event.Metric][key], event.Value)
	}
	return readings
}

func composeContainer(id, service, state, status string, oneOff bool) dockerContainerSummary {
	oneOffLabel := "False"
	if oneOff {
		oneOffLabel = "True"
	}
	return dockerContainerSummary{
		ID:    id,
		Names: []string{"/app-" + id},
		Labels: map[string]string{
			"com.docker.compose.service": service,
			"com.docker.compose.oneoff":  oneOffLabel,
		},
		State:  state,
		Status: status,
	}
}

// A setup job that finished and a `docker compose run` container are not
// services that went down. Reported as down, the first held its alert on
// forever; the second carries the name of a service that is running fine, so a
// leftover one made it read as half down and an open shell in one failed its
// health check.
func TestContainerReadingsLeaveOutContainersNotMeantToRun(t *testing.T) {
	daemon := &fakeDaemon{}
	daemon.set([]dockerContainerSummary{
		composeContainer("web-1", "web", "running", "Up 2 hours", false),
		composeContainer("web-run-1", "web", "exited", "Exited (1) 3 days ago", true),
		composeContainer("web-run-2", "web", "running", "Up 5 minutes", true),
		composeContainer("migrate-1", "migrate", "exited", "Exited (0) 2 hours ago", false),
		// Podman before 6 reports a finished container as stopped.
		composeContainer("seed-1", "seed", "stopped", "Exited (0) 1 minute ago", false),
		composeContainer("worker-1", "worker", "exited", "Exited (137) 5 minutes ago", false),
		// Exit code 0 is only a finished job once the container has stopped; one
		// its restart policy keeps bringing back is flapping.
		composeContainer("cron-1", "cron", "restarting", "Restarting (0) 3 seconds ago", false),
	}, map[string]fakeContainerState{
		"web-run-2": {Health: &fakeHealth{Status: "unhealthy"}},
	})
	c := newFakeDaemonCollector(t, daemon)

	sent := collectRound(t, c)

	wantUp := map[string][]float64{
		"service=web":    {100},
		"service=worker": {0},
		"service=cron":   {0},
	}
	if got := sent["docker.container.up"]; !reflect.DeepEqual(got, wantUp) {
		t.Fatalf("docker.container.up readings = %v, want %v", got, wantUp)
	}
	if got := sent["docker.container.healthy"]; len(got) != 0 {
		t.Fatalf("docker.container.healthy readings = %v, want none: the one-off's check is not web's", got)
	}
	if got := sent["docker.container.mem.usage_kb"]["service=web"]; !reflect.DeepEqual(got, []float64{4}) {
		t.Fatalf("web memory readings = %v, want only web-1's", got)
	}

	// Left off the service lines, but still containers the daemon holds.
	counts := sent["docker.containers_count"]
	for state, want := range map[string]float64{"running": 2, "exited": 3, "restarting": 1, "other": 1} {
		if got := counts["state="+state]; len(got) != 1 || got[0] != want {
			t.Errorf("%s containers counted = %v, want %v", state, got, want)
		}
	}
}

// The exit code comes from human-readable text, so anything that does not read
// exactly as a clean exit keeps the container on the up line. Missing a
// finished job is noise; hiding a crash would be a silent outage.
func TestExitedCleanlyReadsOnlyExitCodeZero(t *testing.T) {
	cases := map[string]bool{
		"Exited (0) 3 hours ago":            true,
		"Exited (0) Less than a second ago": true,
		"  Exited (0) About a minute ago ":  true,
		"Exited (1) 3 hours ago":            false,
		"Exited (137) 5 minutes ago":        false,
		"Exited (-1) 2 days ago":            false,
		"Exited (00) 2 days ago":            false,
		"Restarting (0) 3 seconds ago":      false,
		"Up 2 hours":                        false,
		"Created":                           false,
		"":                                  false,
		"Exited (0":                         false,
		"exited (0) 3 hours ago":            false,
	}
	for status, want := range cases {
		if got := exitedCleanly(status); got != want {
			t.Errorf("exitedCleanly(%q) = %v, want %v", status, got, want)
		}
	}
}

// Docker's out-of-memory flag outlives the kill it records, so reporting it as
// read held "killed for memory" on for as long as the container kept running.
// Each kill must show as exactly one reading of 1.
func TestOOMKilledIsReportedOnceForEachKill(t *testing.T) {
	api := func(state, status string) []dockerContainerSummary {
		return []dockerContainerSummary{composeContainer("api-1", "api", state, status, false)}
	}
	running := api("running", "Up 1 hour")
	started := "2026-09-16T08:00:00.000000001Z"
	restarted := "2026-09-16T09:30:00.000000001Z"
	restartedAgain := "2026-09-16T09:45:00.000000001Z"

	daemon := &fakeDaemon{}
	c := newFakeDaemonCollector(t, daemon)

	steps := []struct {
		name    string
		listing []dockerContainerSummary
		state   fakeContainerState
		want    []float64
	}{
		{"healthy", running, fakeContainerState{StartedAt: started}, []float64{0}},
		// Docker 24 and later: a process inside was killed, the container runs on.
		{"a process is killed", running, fakeContainerState{OOMKilled: true, StartedAt: started}, []float64{1}},
		{"the flag stays set", running, fakeContainerState{OOMKilled: true, StartedAt: started}, []float64{0}},
		// Not running, so not inspected; what was seen must survive the wait.
		{"waiting to restart", api("restarting", "Restarting (137) 1 second ago"), fakeContainerState{}, nil},
		// Engines before 24 carry the flag into the run a restart policy starts,
		// so a second kill shows only as a new start with the flag still set.
		{"killed and restarted", running, fakeContainerState{OOMKilled: true, StartedAt: restarted}, []float64{1}},
		{"restarted for another reason", running, fakeContainerState{StartedAt: restartedAgain}, []float64{0}},
	}
	for _, step := range steps {
		daemon.set(step.listing, map[string]fakeContainerState{"api-1": step.state})
		got := collectRound(t, c)["docker.container.oom_killed"]["service=api"]
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: docker.container.oom_killed = %v, want %v", step.name, got, step.want)
		}
	}

	// A removed container is forgotten, or the memory would grow with every
	// container the host ever ran.
	daemon.set(nil, nil)
	collectRound(t, c)
	if len(c.oomSeen) != 0 {
		t.Fatalf("still remembering %d removed containers", len(c.oomSeen))
	}
}

// The agent starts with no memory of earlier readings. A flag already set then
// belongs to a kill that is most likely old, and counting it would announce the
// same kill again on every agent restart.
func TestOOMKilledSetBeforeTheFirstReadingIsNotAnnounced(t *testing.T) {
	daemon := &fakeDaemon{}
	daemon.set(
		[]dockerContainerSummary{composeContainer("api-1", "api", "running", "Up 3 days", false)},
		map[string]fakeContainerState{"api-1": {OOMKilled: true, StartedAt: "2026-09-13T08:00:00Z"}},
	)
	c := newFakeDaemonCollector(t, daemon)

	for round := 1; round <= 2; round++ {
		got := collectRound(t, c)["docker.container.oom_killed"]["service=api"]
		if !reflect.DeepEqual(got, []float64{0}) {
			t.Fatalf("round %d: docker.container.oom_killed = %v, want [0]", round, got)
		}
	}
}
