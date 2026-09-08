package nginx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

// AccessLogProbe enables access-log reading only when a log is actually readable.
// The agent usually runs as root through systemd, but a user install will not be
// able to open it, and that must be a quiet skip rather than a repeating failure.
type AccessLogProbe struct {
	path       string
	every      time.Duration
	maxSamples int
}

func NewAccessLogProbe(path string, every time.Duration, maxSamples int) *AccessLogProbe {
	if strings.TrimSpace(path) == "" {
		path = DefaultAccessLogPath
	}
	return &AccessLogProbe{path: path, every: every, maxSamples: maxSamples}
}

func (p *AccessLogProbe) ID() string { return "nginx.accesslog" }

func (p *AccessLogProbe) Detect(_ context.Context) (bool, string) {
	file, err := os.Open(p.path)
	if err != nil {
		return false, fmt.Sprintf("open %s: %v", p.path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, fmt.Sprintf("stat %s: %v", p.path, err)
	}
	if info.IsDir() {
		return false, fmt.Sprintf("%s is a directory", p.path)
	}
	return true, fmt.Sprintf("access log readable at %s", p.path)
}

func (p *AccessLogProbe) New() agent.Collector {
	return NewAccessLogCollector(p.path, p.every, p.maxSamples)
}
