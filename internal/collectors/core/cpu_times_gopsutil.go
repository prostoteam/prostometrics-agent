//go:build !linux

package core

import (
	"errors"
	"fmt"

	"github.com/shirou/gopsutil/v3/cpu"
)

func readCPUTimes() (map[string]cpuTimes, error) {
	per, err := cpu.Times(true)
	if err != nil {
		return nil, err
	}
	if len(per) == 0 {
		return nil, errors.New("cpu: no times returned")
	}

	result := make(map[string]cpuTimes, len(per)+1)
	agg := cpuTimes{}

	scale := func(v float64) uint64 {
		if v < 0 {
			return 0
		}
		return uint64(v * 100)
	}

	for i, st := range per {
		ct := cpuTimes{
			user:      scale(st.User),
			nice:      scale(st.Nice),
			system:    scale(st.System),
			idle:      scale(st.Idle),
			iowait:    scale(st.Iowait),
			irq:       scale(st.Irq),
			softirq:   scale(st.Softirq),
			steal:     scale(st.Steal),
			guest:     scale(st.Guest),
			guestNice: scale(st.GuestNice),
		}
		name := fmt.Sprintf("cpu%d", i)
		result[name] = ct

		agg.user += ct.user
		agg.nice += ct.nice
		agg.system += ct.system
		agg.idle += ct.idle
		agg.iowait += ct.iowait
		agg.irq += ct.irq
		agg.softirq += ct.softirq
		agg.steal += ct.steal
		agg.guest += ct.guest
		agg.guestNice += ct.guestNice
	}

	result["cpu"] = agg
	return result, nil
}
