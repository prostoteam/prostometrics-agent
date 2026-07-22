package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type Runner struct {
	sem        chan struct{}
	logLimiter *logLimiter
}

func Run(ctx context.Context, core []Collector, probes []Probe) {
	collectors := make([]Collector, 0, len(core)+len(probes))
	for _, c := range core {
		if c != nil {
			collectors = append(collectors, c)
		}
	}

	for _, p := range probes {
		if p == nil {
			continue
		}
		ok, reason := detectOnce(ctx, p)
		if ok {
			c := newFromProbe(p)
			if c == nil {
				logProbe(p.ID(), true, reason)
				log.Printf("probe %s: init failed", p.ID())
				continue
			}
			logProbe(p.ID(), true, reason)
			collectors = append(collectors, c)
			continue
		}
		logProbe(p.ID(), false, reason)
	}

	r := &Runner{
		sem:        make(chan struct{}, MaxConcurrency),
		logLimiter: newLogLimiter(time.Minute),
	}
	logStartup(collectors)
	r.run(ctx, collectors)
	log.Printf("agent: stopped")
}

func detectOnce(parent context.Context, p Probe) (ok bool, reason string) {
	ctx, cancel := context.WithTimeout(parent, CollectTimeout)
	defer cancel()

	defer func() {
		if v := recover(); v != nil {
			ok = false
			reason = fmt.Sprintf("probe panicked: %v", v)
		}
	}()
	return p.Detect(ctx)
}

func newFromProbe(p Probe) (out Collector) {
	defer func() {
		if v := recover(); v != nil {
			out = nil
		}
	}()
	return p.New()
}

func (r *Runner) run(ctx context.Context, collectors []Collector) {
	var wg sync.WaitGroup
	for _, c := range collectors {
		if c == nil {
			continue
		}
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()
			r.runCollector(ctx, c)
		}(c)
	}

	<-ctx.Done()
	wg.Wait()
	r.closeCollectors(collectors)
}

func (r *Runner) runCollector(ctx context.Context, c Collector) {
	every := c.Every()
	if every <= 0 {
		every = CoreFastEvery
	}

	// Run one collection immediately so slow collectors publish initial points at startup.
	if ctx.Err() == nil {
		r.collectOnce(ctx, c)
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.collectOnce(ctx, c)
		}
	}
}

func (r *Runner) collectOnce(parent context.Context, c Collector) {
	ctx, cancel := context.WithTimeout(parent, CollectTimeout)
	defer cancel()

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.sem }()

	err := safeCollect(ctx, c)
	if err == nil {
		return
	}
	if parent.Err() != nil {
		return
	}

	if r.logLimiter.Allow(c.ID()) {
		log.Printf("collector %s: %v", c.ID(), err)
	}
}

func safeCollect(ctx context.Context, c Collector) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return c.Collect(ctx)
}

func logStartup(collectors []Collector) {
	log.Printf("agent: started (collectors=%d timeout=%s max_concurrency=%d)", len(collectors), CollectTimeout, MaxConcurrency)
	for _, c := range collectors {
		if c == nil {
			continue
		}
		log.Printf("collector %s: every=%s", c.ID(), c.Every())
	}
}

func logProbe(id string, detected bool, reason string) {
	status := "not detected"
	if detected {
		status = "detected"
	}
	if reason == "" {
		log.Printf("probe %s: %s", id, status)
		return
	}
	log.Printf("probe %s: %s: %s", id, status, reason)
}

func (r *Runner) closeCollectors(collectors []Collector) {
	ctx, cancel := context.WithTimeout(context.Background(), CollectTimeout)
	defer cancel()
	for _, c := range collectors {
		if c == nil {
			continue
		}
		closer, ok := c.(CollectorCloser)
		if !ok {
			continue
		}
		func() {
			defer func() {
				if v := recover(); v != nil {
					log.Printf("collector %s: close panic: %v", c.ID(), v)
				}
			}()
			if err := closer.Close(ctx); err != nil {
				log.Printf("collector %s: close failed: %v", c.ID(), err)
			}
		}()
	}
}

type logLimiter struct {
	mu    sync.Mutex
	every time.Duration
	last  map[string]time.Time
}

func newLogLimiter(every time.Duration) *logLimiter {
	return &logLimiter{
		every: every,
		last:  make(map[string]time.Time),
	}
}

func (l *logLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	last, ok := l.last[key]
	if !ok || now.Sub(last) >= l.every {
		l.last[key] = now
		return true
	}
	return false
}
