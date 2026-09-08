package redis

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const defaultOpTimeout = 2 * time.Second

type Instance struct {
	URI   string
	Label string
}

type instanceState struct {
	Instance
	target      dialTarget
	client      *conn
	nextAttempt time.Time
}

// Collector reports the numbers that decide whether Redis is about to start
// losing data: how close it is to its memory ceiling, whether it is already
// evicting keys to stay under it, and whether reads are being served from memory
// or missing entirely.
type Collector struct {
	every      time.Duration
	retryEvery time.Duration
	opTimeout  time.Duration
	instances  []instanceState
}

func NewCollector(instances []Instance, every time.Duration, retryEvery time.Duration) *Collector {
	state := make([]instanceState, 0, len(instances))
	for _, inst := range instances {
		target, err := parseURI(inst.URI)
		if err != nil {
			log.Printf("redis: instance %s: %v", inst.Label, err)
			continue
		}
		state = append(state, instanceState{Instance: inst, target: target})
	}
	if retryEvery <= 0 {
		retryEvery = time.Minute
	}
	return &Collector{
		every:      every,
		retryEvery: retryEvery,
		opTimeout:  defaultOpTimeout,
		instances:  state,
	}
}

func (c *Collector) ID() string { return "redis" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Close(_ context.Context) error {
	for i := range c.instances {
		c.resetClient(&c.instances[i])
	}
	return nil
}

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
			log.Printf("redis: instance %s: %v", inst.Label, err)
			c.resetClient(inst)
			inst.nextAttempt = now.Add(c.retryEvery)
		}
	}
	return nil
}

func (c *Collector) collectInstance(ctx context.Context, inst *instanceState) error {
	client, err := c.ensureClient(ctx, inst)
	if err != nil {
		return err
	}
	raw, err := client.command(c.opTimeout, "INFO", "all")
	if err != nil {
		return fmt.Errorf("INFO: %w", err)
	}
	info := parseInfo(raw)
	instanceLabel := prostometrics.Label("instance", inst.Label)

	emitMemory := func(typ string, key string) uint64 {
		v, ok := infoUint(info, key)
		if !ok {
			return 0
		}
		prostometrics.ValueSparse("redis.memory_kb", float64(v)/1024.0,
			instanceLabel, prostometrics.Label("type", typ),
		)
		return v
	}
	used := emitMemory("used", "used_memory")
	emitMemory("peak", "used_memory_peak")
	emitMemory("rss", "used_memory_rss")
	maxMemory := emitMemory("max", "maxmemory")

	// Without maxmemory Redis grows until the host kills it, so the share is only
	// meaningful — and only reported — when a ceiling was actually configured.
	if maxMemory > 0 && used > 0 {
		pct := 100.0 * float64(used) / float64(maxMemory)
		if pct > 100 {
			pct = 100
		}
		prostometrics.Value("redis.memory_used_pct", pct, instanceLabel)
	}

	emitClients := func(typ string, key string) {
		if v, ok := infoUint(info, key); ok {
			prostometrics.ValueSparse("redis.clients", float64(v),
				instanceLabel, prostometrics.Label("type", typ),
			)
		}
	}
	emitClients("connected", "connected_clients")
	emitClients("blocked", "blocked_clients")

	emitTotal := func(metric string, key string, labels ...string) {
		if v, ok := infoUint(info, key); ok {
			prostometrics.Total(metric, float64(v), append([]string{instanceLabel}, labels...)...)
		}
	}
	emitTotal("redis.keyspace_count", "keyspace_hits", prostometrics.Label("type", "hits"))
	emitTotal("redis.keyspace_count", "keyspace_misses", prostometrics.Label("type", "misses"))
	emitTotal("redis.evicted_keys_count", "evicted_keys")
	emitTotal("redis.expired_keys_count", "expired_keys")
	emitTotal("redis.commands_count", "total_commands_processed")
	emitTotal("redis.connections_count", "total_connections_received", prostometrics.Label("type", "received"))
	emitTotal("redis.connections_count", "rejected_connections", prostometrics.Label("type", "rejected"))

	prostometrics.ValueSparse("redis.keys_count", float64(countKeys(info)), instanceLabel)

	if v, ok := infoUint(info, "rdb_changes_since_last_save"); ok {
		prostometrics.ValueSparse("redis.unsaved_changes", float64(v), instanceLabel)
	}
	if v, ok := infoUint(info, "rdb_last_save_time"); ok && v > 0 {
		age := time.Since(time.Unix(int64(v), 0)).Seconds()
		if age < 0 {
			age = 0
		}
		prostometrics.ValueSparse("redis.last_save_age_sec", age, instanceLabel)
	}

	// Replication numbers exist on a primary too, but only mean something on a
	// replica, where a broken link is silent data staleness.
	if strings.EqualFold(info["role"], "slave") {
		linkUp := 0.0
		if strings.EqualFold(info["master_link_status"], "up") {
			linkUp = 1
		}
		prostometrics.ValueSparse("redis.replica_link_up", linkUp, instanceLabel)
		if v, ok := infoUint(info, "master_last_io_seconds_ago"); ok {
			prostometrics.ValueSparse("redis.replica_lag_sec", float64(v), instanceLabel)
		}
	}

	return nil
}

// countKeys sums the per-database key counts, whose values look like
// "keys=1234,expires=10,avg_ttl=0".
func countKeys(info map[string]string) uint64 {
	var total uint64
	for key, value := range info {
		if !strings.HasPrefix(key, "db") {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			name, raw, ok := strings.Cut(part, "=")
			if !ok || name != "keys" {
				continue
			}
			if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
				total += v
			}
		}
	}
	return total
}

func infoUint(info map[string]string, key string) (uint64, bool) {
	raw, ok := info[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return uint64(v), true
}

func (c *Collector) ensureClient(ctx context.Context, inst *instanceState) (*conn, error) {
	if inst.client != nil {
		return inst.client, nil
	}
	client, err := dial(ctx, inst.target, c.opTimeout)
	if err != nil {
		return nil, err
	}
	inst.client = client
	return client, nil
}

func (c *Collector) resetClient(inst *instanceState) {
	if inst.client == nil {
		return
	}
	_ = inst.client.Close()
	inst.client = nil
}
