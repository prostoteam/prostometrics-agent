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

	"github.com/shirou/gopsutil/v3/load"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// LoadAvgCollector reports run-queue averages divided by the number of
// processors. The normalized form is comparable between machines, which is
// what both the Host card and its shipped alert threshold need.
type LoadAvgCollector struct {
	every time.Duration
	cpus  int
}

func NewLoadAvg(every time.Duration) *LoadAvgCollector {
	cpus := runtime.NumCPU()
	if cpus < 1 {
		cpus = 1
	}
	return &LoadAvgCollector{every: every, cpus: cpus}
}

func (c *LoadAvgCollector) ID() string { return "core.loadavg" }

func (c *LoadAvgCollector) Every() time.Duration { return c.every }

func (c *LoadAvgCollector) Collect(_ context.Context) error {
	avg, err := readLoadAvg()
	if err != nil {
		return err
	}

	emit := func(window string, v float64) {
		if v < 0 {
			return
		}
		prostometrics.Value("host.load_per_core", v/float64(c.cpus), prostometrics.Label("window", window))
	}
	emit("1m", avg.load1)
	emit("5m", avg.load5)
	emit("15m", avg.load15)

	return nil
}

type loadAvgSample struct {
	load1  float64
	load5  float64
	load15 float64
}

func readLoadAvg() (loadAvgSample, error) {
	if runtime.GOOS != "linux" {
		stat, err := load.Avg()
		if err != nil {
			return loadAvgSample{}, fmt.Errorf("load.Avg: %w", err)
		}
		return loadAvgSample{load1: stat.Load1, load5: stat.Load5, load15: stat.Load15}, nil
	}

	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return loadAvgSample{}, fmt.Errorf("open /proc/loadavg: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return loadAvgSample{}, fmt.Errorf("scan /proc/loadavg: %w", err)
		}
		return loadAvgSample{}, errors.New("empty /proc/loadavg")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return loadAvgSample{}, errors.New("invalid /proc/loadavg")
	}
	out := loadAvgSample{}
	for i, target := range []*float64{&out.load1, &out.load5, &out.load15} {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return loadAvgSample{}, fmt.Errorf("parse /proc/loadavg field %d: %w", i, err)
		}
		*target = v
	}
	return out, nil
}
