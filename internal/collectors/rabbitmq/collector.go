package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	defaultOpTimeout = 3 * time.Second
	maxBodyBytes     = 8 << 20

	// maxQueues bounds the per-queue breakdown. Queue names are frequently
	// generated per consumer, so an unbounded label here would turn into an
	// identifier and make the chart unreadable as well as expensive.
	maxQueues = 20
)

type Instance struct {
	URL   string
	Label string
}

type instanceState struct {
	Instance
	nextAttempt time.Time
	lastQueues  map[string]struct{}
}

// Collector reports RabbitMQ through its management API. The reading that matters
// is queue depth against consumer count: a queue growing while consumers sit at
// zero is the shape of every stuck asynchronous pipeline, and no host metric shows it.
type Collector struct {
	every      time.Duration
	retryEvery time.Duration
	client     *http.Client
	instances  []instanceState
}

func NewCollector(instances []Instance, every time.Duration, retryEvery time.Duration) *Collector {
	state := make([]instanceState, len(instances))
	for i, inst := range instances {
		state[i] = instanceState{Instance: inst, lastQueues: make(map[string]struct{})}
	}
	if retryEvery <= 0 {
		retryEvery = time.Minute
	}
	return &Collector{
		every:      every,
		retryEvery: retryEvery,
		client:     &http.Client{Timeout: defaultOpTimeout},
		instances:  state,
	}
}

func (c *Collector) ID() string { return "rabbitmq" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Collect(ctx context.Context) error {
	now := time.Now()
	for i := range c.instances {
		inst := &c.instances[i]
		if now.Before(inst.nextAttempt) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := c.collectInstance(ctx, inst); err != nil {
			log.Printf("rabbitmq: instance %s: %v", inst.Label, err)
			inst.nextAttempt = now.Add(c.retryEvery)
		}
	}
	return nil
}

type overview struct {
	ObjectTotals struct {
		Connections int64 `json:"connections"`
		Channels    int64 `json:"channels"`
		Queues      int64 `json:"queues"`
		Consumers   int64 `json:"consumers"`
	} `json:"object_totals"`
	QueueTotals struct {
		Messages       int64 `json:"messages"`
		Ready          int64 `json:"messages_ready"`
		Unacknowledged int64 `json:"messages_unacknowledged"`
	} `json:"queue_totals"`
	MessageStats struct {
		Publish    int64 `json:"publish"`
		DeliverGet int64 `json:"deliver_get"`
		Ack        int64 `json:"ack"`
		Redeliver  int64 `json:"redeliver"`
	} `json:"message_stats"`
}

type queueInfo struct {
	Name           string `json:"name"`
	VHost          string `json:"vhost"`
	Ready          int64  `json:"messages_ready"`
	Unacknowledged int64  `json:"messages_unacknowledged"`
	Consumers      int64  `json:"consumers"`
}

func (c *Collector) collectInstance(ctx context.Context, inst *instanceState) error {
	instanceLabel := prostometrics.Label("instance", inst.Label)

	var view overview
	if err := c.getJSON(ctx, inst.URL, "/api/overview", &view); err != nil {
		return err
	}

	emitObject := func(typ string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.ValueSparse("rabbitmq.objects", float64(v),
			instanceLabel, prostometrics.Label("type", typ),
		)
	}
	emitObject("connections", view.ObjectTotals.Connections)
	emitObject("channels", view.ObjectTotals.Channels)
	emitObject("queues", view.ObjectTotals.Queues)
	emitObject("consumers", view.ObjectTotals.Consumers)

	emitMessages := func(state string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.ValueSparse("rabbitmq.messages", float64(v),
			instanceLabel, prostometrics.Label("state", state),
		)
	}
	emitMessages("ready", view.QueueTotals.Ready)
	emitMessages("unacknowledged", view.QueueTotals.Unacknowledged)

	emitFlow := func(typ string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.Total("rabbitmq.messages_count", float64(v),
			instanceLabel, prostometrics.Label("type", typ),
		)
	}
	emitFlow("publish", view.MessageStats.Publish)
	emitFlow("deliver", view.MessageStats.DeliverGet)
	emitFlow("ack", view.MessageStats.Ack)
	emitFlow("redeliver", view.MessageStats.Redeliver)

	queues, err := c.listQueues(ctx, inst.URL)
	if err != nil {
		// Overview already published; a restricted management user that cannot
		// list queues should not lose the totals as well.
		return nil
	}

	sort.Slice(queues, func(i, j int) bool {
		return queues[i].Ready+queues[i].Unacknowledged > queues[j].Ready+queues[j].Unacknowledged
	})
	if len(queues) > maxQueues {
		queues = queues[:maxQueues]
	}

	current := make(map[string]struct{}, len(queues))
	for _, queue := range queues {
		name := strings.TrimSpace(queue.Name)
		if name == "" {
			continue
		}
		current[name] = struct{}{}
		queueLabel := prostometrics.Label("queue", name)
		prostometrics.ValueSparse("rabbitmq.queue_messages", float64(max64(queue.Ready, 0)),
			instanceLabel, queueLabel, prostometrics.Label("state", "ready"),
		)
		prostometrics.ValueSparse("rabbitmq.queue_messages", float64(max64(queue.Unacknowledged, 0)),
			instanceLabel, queueLabel, prostometrics.Label("state", "unacknowledged"),
		)
		prostometrics.ValueSparse("rabbitmq.queue_consumers", float64(max64(queue.Consumers, 0)),
			instanceLabel, queueLabel,
		)
	}
	// A queue that drained out of the top list must report zero rather than keep
	// its last depth forever, which would read as a permanent backlog.
	for name := range inst.lastQueues {
		if _, still := current[name]; still {
			continue
		}
		queueLabel := prostometrics.Label("queue", name)
		prostometrics.ValueSparse("rabbitmq.queue_messages", 0,
			instanceLabel, queueLabel, prostometrics.Label("state", "ready"),
		)
		prostometrics.ValueSparse("rabbitmq.queue_messages", 0,
			instanceLabel, queueLabel, prostometrics.Label("state", "unacknowledged"),
		)
	}
	inst.lastQueues = current

	return nil
}

// listQueues asks for the sorted, paginated form first, then falls back to the
// plain array older management plugins return.
func (c *Collector) listQueues(ctx context.Context, base string) ([]queueInfo, error) {
	const columns = "name,vhost,messages_ready,messages_unacknowledged,consumers"
	paths := []string{
		fmt.Sprintf("/api/queues?columns=%s&page=1&page_size=%d&sort=messages&sort_reverse=true",
			url.QueryEscape(columns), maxQueues),
		"/api/queues?columns=" + url.QueryEscape(columns),
	}

	var lastErr error
	for _, path := range paths {
		var raw json.RawMessage
		if err := c.getJSON(ctx, base, path, &raw); err != nil {
			lastErr = err
			continue
		}
		var page struct {
			Items []queueInfo `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err == nil && page.Items != nil {
			return page.Items, nil
		}
		var list []queueInfo
		if err := json.Unmarshal(raw, &list); err == nil {
			return list, nil
		}
		lastErr = fmt.Errorf("unrecognized /api/queues response")
	}
	return nil, lastErr
}

func (c *Collector) getJSON(ctx context.Context, base string, path string, dst any) error {
	endpoint := strings.TrimSuffix(base, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// Credentials travel in the URL's user info; move them to the header so they
	// never appear in a proxy or server access log.
	if req.URL.User != nil {
		password, _ := req.URL.User.Password()
		req.SetBasicAuth(req.URL.User.Username(), password)
		req.URL.User = nil
	}
	req.Header.Set("User-Agent", "prostometrics-rabbitmq-collector")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request %s: http %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(dst)
}

func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}
