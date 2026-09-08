package prom

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	defaultTimeout = 5 * time.Second
	maxBodyBytes   = 16 << 20

	// DefaultMaxSeries caps what one target may publish. Exporters routinely
	// expose thousands of series, and forwarding them all would cost more than
	// every other metric on the host combined, so a target must say what it wants
	// rather than send everything.
	DefaultMaxSeries = 100
)

// Target is one scrape endpoint. Every target is scraped on the collector's
// single cadence; there is deliberately no per-target interval, because the
// runner drives one collection per collector and a second schedule inside it
// would need its own timer and its own failure modes.
type Target struct {
	Name      string
	URL       string
	Metrics   []string
	Labels    []string
	MaxSeries int
}

// Collector reads any endpoint that speaks the Prometheus text format and
// forwards a declared subset. It exists so that a service the agent has no
// dedicated support for — a reverse proxy, a queue exporter, an application's own
// endpoint — can be reported without waiting for a new agent release.
type Collector struct {
	every   time.Duration
	client  *http.Client
	targets []Target
	warned  map[string]bool
}

func NewCollector(targets []Target, every time.Duration) *Collector {
	normalized := make([]Target, 0, len(targets))
	for _, target := range targets {
		if target.MaxSeries <= 0 {
			target.MaxSeries = DefaultMaxSeries
		}
		normalized = append(normalized, target)
	}
	return &Collector{
		every:   every,
		client:  &http.Client{Timeout: defaultTimeout},
		targets: normalized,
		warned:  make(map[string]bool),
	}
}

func (c *Collector) ID() string { return "prometheus" }

func (c *Collector) Every() time.Duration { return c.every }

// Timeout asks the runner for a deadline that covers every target. Targets are
// scraped one after another, so the default collection deadline would be spent
// on the first slow one and leave the targets behind it unscraped in every
// round — reporting nothing at all rather than reporting late.
func (c *Collector) Timeout() time.Duration {
	if len(c.targets) == 0 {
		return defaultTimeout
	}
	return defaultTimeout * time.Duration(len(c.targets))
}

func (c *Collector) Collect(ctx context.Context) error {
	for _, target := range c.targets {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.scrape(ctx, target); err != nil {
			log.Printf("prometheus: target %s: %v", target.Name, err)
		}
	}
	return nil
}

func (c *Collector) scrape(parent context.Context, target Target) error {
	// Each target gets its own slice of the budget, so one endpoint that hangs
	// cannot consume the time the others need.
	ctx, cancel := context.WithTimeout(parent, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if req.URL.User != nil {
		password, _ := req.URL.User.Password()
		req.SetBasicAuth(req.URL.User.Username(), password)
		req.URL.User = nil
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	req.Header.Set("User-Agent", "prostometrics-prometheus-collector")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	allowLabel := matcher(target.Labels)
	samples, err := parseExposition(io.LimitReader(resp.Body, maxBodyBytes), allowLabel)
	if err != nil {
		return err
	}

	allowMetric := matcher(target.Metrics)
	targetLabel := prostometrics.Label("target", target.Name)
	published := 0
	for _, s := range samples {
		if !allowMetric(s.name) {
			continue
		}
		if published >= target.MaxSeries {
			if !c.warned[target.Name] {
				c.warned[target.Name] = true
				log.Printf("prometheus: target %s: stopped at %d series; narrow `metrics` or raise `max_series`",
					target.Name, target.MaxSeries)
			}
			break
		}
		// Ingest rejects negatives, and a counter is never legitimately negative;
		// a gauge that is means something the product cannot chart anyway.
		if s.value < 0 {
			continue
		}
		name := metricName(s.name)
		if name == "" {
			continue
		}
		labels := append([]string{targetLabel}, s.labels...)
		if s.kind == "counter" {
			prostometrics.Total(name, s.value, labels...)
		} else {
			prostometrics.ValueSparse(name, s.value, labels...)
		}
		published++
	}
	return nil
}

// metricName keeps the exporter's own name so the endpoint's documentation still
// applies, and drops anything ingest would reject.
func metricName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "|\r\n") {
		return ""
	}
	return name
}

// matcher builds an allowlist test. An empty list allows everything, and a
// pattern may end in '*' to accept a prefix, which is how exporters group a
// family of related series.
func matcher(patterns []string) func(string) bool {
	if len(patterns) == 0 {
		return func(string) bool { return true }
	}
	cleaned := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return func(string) bool { return true }
	}
	return func(candidate string) bool {
		for _, pattern := range cleaned {
			if pattern == candidate {
				return true
			}
			if strings.HasSuffix(pattern, "*") && strings.HasPrefix(candidate, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			if ok, err := path.Match(pattern, candidate); err == nil && ok {
				return true
			}
		}
		return false
	}
}
