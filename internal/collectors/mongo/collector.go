package mongo

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const defaultOpTimeout = 2 * time.Second

type Instance struct {
	URI   string
	Label string
}

type instanceState struct {
	Instance
	client      *mongo.Client
	nextAttempt time.Time
	prevOpLat   opLatencies
	hasPrevLat  bool
}

type Collector struct {
	every      time.Duration
	retryEvery time.Duration
	opTimeout  time.Duration
	instances  []instanceState
}

func NewCollector(instances []Instance, every time.Duration, retryEvery time.Duration) *Collector {
	state := make([]instanceState, len(instances))
	for i, inst := range instances {
		state[i] = instanceState{Instance: inst}
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

func (c *Collector) ID() string { return "mongo" }

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
			log.Printf("mongo: instance %s: %v", inst.Label, err)
			inst.nextAttempt = now.Add(c.retryEvery)
		}
	}
	return nil
}

func (c *Collector) Close(ctx context.Context) error {
	for i := range c.instances {
		c.resetClient(ctx, &c.instances[i])
	}
	return nil
}

type serverStatus struct {
	Connections struct {
		Current   int64 `bson:"current"`
		Available int64 `bson:"available"`
	} `bson:"connections"`
	Mem         memStats         `bson:"mem"`
	WiredTiger  wiredTigerStatus `bson:"wiredTiger"`
	OpCounters  opCounters       `bson:"opcounters"`
	OpLatencies opLatencies      `bson:"opLatencies"`
}

type opCounters struct {
	Insert  int64 `bson:"insert"`
	Query   int64 `bson:"query"`
	Update  int64 `bson:"update"`
	Delete  int64 `bson:"delete"`
	GetMore int64 `bson:"getmore"`
	Command int64 `bson:"command"`
}

type opLatencies struct {
	Reads    opLatency `bson:"reads"`
	Writes   opLatency `bson:"writes"`
	Commands opLatency `bson:"commands"`
}

type opLatency struct {
	Latency int64 `bson:"latency"`
	Ops     int64 `bson:"ops"`
}

type memStats struct {
	Resident int64 `bson:"resident"`
}

type wiredTigerStatus struct {
	Cache wiredTigerCache `bson:"cache"`
}

type wiredTigerCache struct {
	BytesCurrent int64 `bson:"bytes currently in the cache"`
	BytesMax     int64 `bson:"maximum bytes configured"`
	Evictions    int64 `bson:"evictions"`
}

func (c *Collector) collectInstance(ctx context.Context, inst *instanceState) error {
	timeout := c.opTimeout
	if timeout <= 0 {
		timeout = defaultOpTimeout
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := c.ensureClient(opCtx, inst, timeout)
	if err != nil {
		return err
	}

	var status serverStatus
	if err := client.Database("admin").RunCommand(opCtx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&status); err != nil {
		c.resetClient(context.Background(), inst)
		return fmt.Errorf("serverStatus: %w", err)
	}

	instanceLabel := prostometrics.Label("instance", inst.Label)
	emitConnection := func(typ string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.Value("mongo.connections", float64(v),
			instanceLabel,
			prostometrics.Label("type", typ),
		)
	}
	emitConnection("current", status.Connections.Current)
	emitConnection("available", status.Connections.Available)
	if status.Mem.Resident >= 0 {
		prostometrics.Value("mongo.mem.resident_mb", float64(status.Mem.Resident), instanceLabel)
	}
	emitWTCache := func(typ string, bytes int64) {
		if bytes < 0 {
			return
		}
		kb := float64(bytes) / 1024.0
		prostometrics.Value("mongo.wt.cache.kb", kb,
			instanceLabel,
			prostometrics.Label("type", typ),
		)
	}
	emitWTCache("used", status.WiredTiger.Cache.BytesCurrent)
	emitWTCache("max", status.WiredTiger.Cache.BytesMax)
	if status.WiredTiger.Cache.Evictions >= 0 {
		prostometrics.Total("mongo.wt.cache.evictions_count", float64(status.WiredTiger.Cache.Evictions), instanceLabel)
	}

	emitOp := func(typ string, total int64) {
		if total < 0 {
			return
		}
		prostometrics.Total("mongo.ops_count", float64(total),
			instanceLabel,
			prostometrics.Label("type", typ),
		)
	}
	emitOp("insert", status.OpCounters.Insert)
	emitOp("query", status.OpCounters.Query)
	emitOp("update", status.OpCounters.Update)
	emitOp("delete", status.OpCounters.Delete)
	emitOp("getmore", status.OpCounters.GetMore)
	emitOp("command", status.OpCounters.Command)

	if inst.hasPrevLat {
		emitLatency := func(typ string, prev, cur opLatency) {
			deltaOps := diffCounter(prev.Ops, cur.Ops)
			if deltaOps <= 0 {
				return
			}
			deltaLatency := diffCounter(prev.Latency, cur.Latency)
			if deltaLatency <= 0 {
				return
			}
			avgMs := float64(deltaLatency) / float64(deltaOps) / 1000.0
			if avgMs <= 0 {
				return
			}
			prostometrics.Value("mongo.op_latency_ms", avgMs,
				instanceLabel,
				prostometrics.Label("type", typ),
			)
		}
		emitLatency("reads", inst.prevOpLat.Reads, status.OpLatencies.Reads)
		emitLatency("writes", inst.prevOpLat.Writes, status.OpLatencies.Writes)
		emitLatency("commands", inst.prevOpLat.Commands, status.OpLatencies.Commands)
	}
	inst.prevOpLat = status.OpLatencies
	inst.hasPrevLat = true
	return nil
}

func diffCounter(prev, cur int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return 0
}

func (c *Collector) ensureClient(ctx context.Context, inst *instanceState, timeout time.Duration) (*mongo.Client, error) {
	if inst.client != nil {
		return inst.client, nil
	}
	opts := options.Client().ApplyURI(inst.URI)
	opts.SetConnectTimeout(timeout)
	opts.SetServerSelectionTimeout(timeout)
	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("ping: %w", err)
	}
	inst.client = client
	return client, nil
}

func (c *Collector) resetClient(ctx context.Context, inst *instanceState) {
	if inst.client == nil {
		return
	}
	disconnectCtx := ctx
	if disconnectCtx == nil {
		disconnectCtx = context.Background()
	}
	if _, ok := disconnectCtx.Deadline(); !ok {
		var cancel context.CancelFunc
		disconnectCtx, cancel = context.WithTimeout(disconnectCtx, time.Second)
		defer cancel()
	}
	_ = inst.client.Disconnect(disconnectCtx)
	inst.client = nil
}
