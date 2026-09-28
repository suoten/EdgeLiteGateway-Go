package api

import (
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

var (
	netMu       sync.Mutex
	lastNetSent uint64
	lastNetRecv uint64
	lastNetTime time.Time
)

// cpuPercentCached returns overall CPU usage. gopsutil keeps the previous
// sample internally, so the first call returns 0 and later calls return the
// real delta since the previous call.
func cpuPercentCached() float64 {
	p, err := cpu.Percent(0, false)
	if err != nil || len(p) == 0 {
		return 0
	}
	return p[0]
}

func cpuCoreCount() int {
	n, err := cpu.Counts(true)
	if err != nil || n <= 0 {
		return runtime.NumCPU()
	}
	return n
}

func cpuFreqMHz() float64 {
	infos, err := cpu.Info()
	if err != nil || len(infos) == 0 {
		return 0
	}
	return infos[0].Mhz
}

func memorySnapshot() (total, used uint64, percent float64) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, 0
	}
	return vm.Total, vm.Used, vm.UsedPercent
}

func diskSnapshot(path string) (total, used uint64, percent float64) {
	du, err := disk.Usage(path)
	if err != nil {
		return 0, 0, 0
	}
	return du.Total, du.Used, du.UsedPercent
}

// netIOSnapshot returns MB transferred since the previous call, so polled
// charts show per-interval traffic instead of a monotonically growing counter.
func netIOSnapshot() (sentMB, recvMB float64) {
	netMu.Lock()
	defer netMu.Unlock()
	counters, err := net.IOCounters(false)
	if err != nil || len(counters) == 0 {
		return 0, 0
	}
	agg := counters[0]
	now := time.Now()
	if !lastNetTime.IsZero() {
		if agg.BytesSent >= lastNetSent {
			sentMB = float64(agg.BytesSent-lastNetSent) / 1024 / 1024
		}
		if agg.BytesRecv >= lastNetRecv {
			recvMB = float64(agg.BytesRecv-lastNetRecv) / 1024 / 1024
		}
	}
	lastNetSent = agg.BytesSent
	lastNetRecv = agg.BytesRecv
	lastNetTime = now
	return
}
