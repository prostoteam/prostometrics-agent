package nginx

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

type Probe struct {
	endpoint string
	every    time.Duration
}

func NewProbe(endpoint string, every time.Duration) *Probe {
	return &Probe{endpoint: endpoint, every: every}
}

func (p *Probe) ID() string { return "nginx" }

func (p *Probe) Detect(ctx context.Context) (bool, string) {
	endpoint := strings.TrimSpace(p.endpoint)
	if endpoint == "" {
		found, err := autoProbeEndpoint(ctx)
		if err != nil {
			return false, err.Error()
		}
		p.endpoint = found
		return true, fmt.Sprintf("stub_status autoprobed at %s", found)
	}
	_, err := fetchStubStatus(ctx, &http.Client{Timeout: defaultTimeout}, endpoint)
	if err != nil {
		return false, err.Error()
	}
	return true, fmt.Sprintf("stub_status reachable at %s", endpoint)
}

func (p *Probe) New() agent.Collector {
	return NewCollector(p.endpoint, p.every)
}

var (
	autoProbePorts = []int{80, 8080, 8081, 8888}
	autoProbePaths = []string{"/stub_status", "/nginx_status"}
)

func autoProbeEndpoint(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: defaultTimeout}
	var lastErr error
	for _, port := range autoProbePorts {
		for _, path := range autoProbePaths {
			endpoint := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
			if _, err := fetchStubStatus(ctx, client, endpoint); err == nil {
				return endpoint, nil
			} else {
				lastErr = err
			}
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("nginx autoprobe failed: %w", lastErr)
	}
	return "", fmt.Errorf("nginx autoprobe failed: no endpoints tried")
}
