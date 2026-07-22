//go:build linux

package core

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

func readCPUTimes() (map[string]cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]cpuTimes)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		name := fields[0]

		var vals [10]uint64
		for i := 0; i < len(vals) && 1+i < len(fields); i++ {
			v, err := parseUint(fields[1+i])
			if err != nil {
				v = 0
			}
			vals[i] = v
		}

		result[name] = cpuTimes{
			user:      vals[0],
			nice:      vals[1],
			system:    vals[2],
			idle:      vals[3],
			iowait:    vals[4],
			irq:       vals[5],
			softirq:   vals[6],
			steal:     vals[7],
			guest:     vals[8],
			guestNice: vals[9],
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("no cpu lines in /proc/stat")
	}
	if _, ok := result["cpu"]; !ok {
		return nil, fmt.Errorf("missing aggregate cpu line: %w", errNoCPUs)
	}
	return result, nil
}
