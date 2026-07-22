package nginx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

const (
	DefaultEndpoint = "http://127.0.0.1/stub_status"
	maxBodyBytes    = 4096
	defaultTimeout  = 2 * time.Second
)

type Collector struct {
	every    time.Duration
	endpoint string
	client   *http.Client
}

func NewCollector(endpoint string, every time.Duration) *Collector {
	normalized := NormalizeEndpoint(endpoint)
	return &Collector{
		every:    every,
		endpoint: normalized,
		client: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

func (c *Collector) ID() string { return "nginx" }

func (c *Collector) Every() time.Duration { return c.every }

func (c *Collector) Collect(ctx context.Context) error {
	status, err := fetchStubStatus(ctx, c.client, c.endpoint)
	if err != nil {
		return err
	}

	emitConn := func(state string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.Value("nginx.connections", float64(v), prostometrics.Label("state", state))
	}
	emitConn("active", status.Active)
	emitConn("reading", status.Reading)
	emitConn("writing", status.Writing)
	emitConn("waiting", status.Waiting)

	emitTotal := func(typ string, v int64) {
		if v < 0 {
			return
		}
		prostometrics.Total("nginx.totals", float64(v), prostometrics.Label("type", typ))
	}
	emitTotal("accepts", status.Accepts)
	emitTotal("handled", status.Handled)
	emitTotal("requests", status.Requests)

	return nil
}

type stubStatus struct {
	Active   int64
	Accepts  int64
	Handled  int64
	Requests int64
	Reading  int64
	Writing  int64
	Waiting  int64
}

// NormalizeEndpoint returns a normalized stub_status endpoint string.
func NormalizeEndpoint(endpoint string) string {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return DefaultEndpoint
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	parsed, err := url.Parse(ep)
	if err != nil {
		return ep
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/stub_status"
	}
	return parsed.String()
}

// ValidateEndpoint ensures the endpoint is a valid URL with a host.
func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("missing host in %q", endpoint)
	}
	return nil
}

func parseStubStatus(raw string) (stubStatus, error) {
	var out stubStatus
	var haveActive, haveTotals, haveStates bool
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "Active") && strings.HasPrefix(strings.ToLower(fields[1]), "connections") {
			v, ok := parseInt(fields[len(fields)-1])
			if ok {
				out.Active = v
				haveActive = true
			}
			continue
		}
		if len(fields) >= 3 {
			a, okA := parseInt(fields[0])
			h, okH := parseInt(fields[1])
			r, okR := parseInt(fields[2])
			if okA && okH && okR {
				out.Accepts = a
				out.Handled = h
				out.Requests = r
				haveTotals = true
				continue
			}
		}

		reading, okR := parseKey(fields, "Reading")
		writing, okW := parseKey(fields, "Writing")
		waiting, okWa := parseKey(fields, "Waiting")
		if okR || okW || okWa {
			if okR {
				out.Reading = reading
			}
			if okW {
				out.Writing = writing
			}
			if okWa {
				out.Waiting = waiting
			}
			if okR && okW && okWa {
				haveStates = true
			}
		}
	}
	if !haveActive || !haveTotals || !haveStates {
		return out, fmt.Errorf("incomplete stub_status (active=%t totals=%t states=%t)", haveActive, haveTotals, haveStates)
	}
	return out, nil
}

func fetchStubStatus(ctx context.Context, client *http.Client, endpoint string) (stubStatus, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultEndpoint
	}
	c := client
	if c == nil {
		c = &http.Client{Timeout: defaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return stubStatus{}, fmt.Errorf("nginx: build request: %w", err)
	}
	req.Header.Set("User-Agent", "prostometrics-nginx-collector")
	resp, err := c.Do(req)
	if err != nil {
		return stubStatus{}, fmt.Errorf("nginx: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return stubStatus{}, fmt.Errorf("nginx: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return stubStatus{}, fmt.Errorf("nginx: read body: %w", err)
	}
	status, err := parseStubStatus(string(body))
	if err != nil {
		return stubStatus{}, fmt.Errorf("nginx: parse stub_status: %w", err)
	}
	return status, nil
}

func parseKey(fields []string, key string) (int64, bool) {
	needle := key + ":"
	for i, f := range fields {
		if f == needle {
			if i+1 >= len(fields) {
				return 0, false
			}
			return parseInt(fields[i+1])
		}
		if strings.HasPrefix(f, needle) {
			return parseInt(strings.TrimPrefix(f, needle))
		}
	}
	return 0, false
}

func parseInt(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
