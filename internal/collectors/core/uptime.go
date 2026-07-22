package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/host"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

type UptimeCollector struct {
	every time.Duration
}

func NewUptime(every time.Duration) *UptimeCollector {
	return &UptimeCollector{
		every: every,
	}
}

func (c *UptimeCollector) ID() string { return "core.uptime" }

func (c *UptimeCollector) Every() time.Duration { return c.every }

func (c *UptimeCollector) Collect(_ context.Context) error {
	uptimeSec, err := readUptimeSeconds()
	if err != nil {
		return err
	}
	if uptimeSec < 0 {
		return nil
	}
	prostometrics.ValueSparse("host.uptime_min", uptimeSec/60.0)
	return nil
}

func readUptimeSeconds() (float64, error) {
	if runtime.GOOS != "linux" {
		uptime, err := host.Uptime()
		if err != nil {
			return 0, fmt.Errorf("uptime: %w", err)
		}
		return float64(uptime), nil
	}

	f, err := os.Open("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("open /proc/uptime: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, fmt.Errorf("scan /proc/uptime: %w", err)
		}
		return 0, errors.New("empty /proc/uptime")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return 0, errors.New("invalid /proc/uptime")
	}
	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime: %w", err)
	}
	return val, nil
}
