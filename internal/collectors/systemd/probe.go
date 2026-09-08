package systemd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
)

const (
	defaultBinary = "systemctl"
	// bootedMarker is the canonical test for "this host was booted by systemd";
	// systemctl being installed is not the same thing, for instance inside a
	// container that merely has the package.
	bootedMarker = "/run/systemd/system"
)

type Probe struct {
	every  time.Duration
	binary string
}

func NewProbe(every time.Duration) *Probe { return &Probe{every: every} }

func (p *Probe) ID() string { return "systemd" }

func (p *Probe) Detect(ctx context.Context) (bool, string) {
	if _, err := os.Stat(bootedMarker); err != nil {
		return false, fmt.Sprintf("stat %s: %v", bootedMarker, err)
	}
	path, err := exec.LookPath(defaultBinary)
	if err != nil {
		return false, fmt.Sprintf("look up %s: %v", defaultBinary, err)
	}
	p.binary = path

	probe := NewCollector(path, p.every)
	if _, err := probe.listUnits(ctx, "failed"); err != nil {
		return false, err.Error()
	}
	return true, fmt.Sprintf("systemd booted, using %s", path)
}

func (p *Probe) New() agent.Collector { return NewCollector(p.binary, p.every) }
