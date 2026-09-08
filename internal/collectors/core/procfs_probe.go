package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

// FileProbe enables a collector only when the kernel files it reads exist. The
// Linux-only collectors are registered through it so a macOS development run,
// or a kernel built without an option, stays quiet instead of logging the same
// failure once a minute forever.
type FileProbe struct {
	id    string
	paths []string
	every time.Duration
	build func(time.Duration) agent.Collector
}

func NewFileProbe(id string, paths []string, every time.Duration, build func(time.Duration) agent.Collector) *FileProbe {
	return &FileProbe{id: id, paths: paths, every: every, build: build}
}

func (p *FileProbe) ID() string { return p.id }

func (p *FileProbe) Detect(_ context.Context) (bool, string) {
	for _, path := range p.paths {
		if _, err := os.Stat(path); err == nil {
			return true, fmt.Sprintf("%s is readable", path)
		}
	}
	return false, fmt.Sprintf("none of %s exist", strings.Join(p.paths, ", "))
}

func (p *FileProbe) New() agent.Collector { return p.build(p.every) }
