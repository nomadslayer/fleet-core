package agent

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fleetcore/internal/api"
)

// metricsLoop pushes live samples at MetricsEvery intervals. CPU and
// network are delta-based, so the loop keeps last readings between
// ticks. All values come from /proc and statfs — no subprocesses.
func (a *Agent) metricsLoop(ctx context.Context) {
	if a.MetricsEvery <= 0 {
		return
	}
	var (
		prevBusy, prevTotal uint64
		prevRx, prevTx      uint64
		prevAt              time.Time
	)
	prevBusy, prevTotal = readCPU()
	prevRx, prevTx = readNet()
	prevAt = time.Now()
	prevProcs := map[int]procCPU{}
	prevIfaces := readNetPerIface()
	ncpu := runtime.NumCPU()
	ks := &kubeState{}

	t := time.NewTicker(a.MetricsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			busy, total := readCPU()
			rx, tx := readNet()
			now := time.Now()
			elapsed := now.Sub(prevAt).Seconds()
			totalDelta := total - prevTotal

			s := api.MetricsSample{AtUnix: now.Unix()}
			if totalDelta > 0 {
				s.CPUPercent = 100 * float64(busy-prevBusy) / float64(totalDelta)
			}
			if elapsed > 0 {
				s.NetRxBps = float64(rx-prevRx) / elapsed
				s.NetTxBps = float64(tx-prevTx) / elapsed
			}
			s.Load1 = readLoad1()
			s.MemTotal, s.MemUsed = readMem()
			s.DiskTotal, s.DiskUsed = readDisk("/")

			rep := processReport(prevProcs, totalDelta, ncpu, a.topN())
			s.TopProcesses, s.ProcTotalRSS, s.ProcCount = rep.Samples, rep.TotalRSS, rep.Count

			curIfaces := readNetPerIface()
			s.Interfaces = netSamples(prevIfaces, curIfaces, elapsed)
			prevIfaces = curIfaces

			// Pod collection is self-disabling: on a non-Kubernetes host the
			// kubelet probe fails once and the cgroup walk finds no kubepods
			// tree, so this costs a single stat() per tick.
			s.Pods = a.collectPods(ks, elapsed)

			prevBusy, prevTotal, prevRx, prevTx, prevAt = busy, total, rx, tx, now

			if err := a.postJSON(ctx, "/v1/metrics", s, nil); err != nil {
				a.Log.Warn("metrics push failed", "err", err)
			}
		}
	}
}

// topN is how many processes to include in the htop-style breakdown, per
// ranking — the report unions top-N by CPU with top-N by memory and every
// recognised service, so the emitted list is usually larger than this.
func (a *Agent) topN() int {
	if a.TopProcesses > 0 {
		return a.TopProcesses
	}
	return 15
}

// readCPU returns aggregate busy and total jiffies from /proc/stat.
func readCPU() (busy, total uint64) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		vals = append(vals, v)
	}
	for _, v := range vals {
		total += v
	}
	idle := vals[3] // idle
	if len(vals) > 4 {
		idle += vals[4] // + iowait
	}
	return total - idle, total
}

func readLoad1() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	if fields := strings.Fields(string(raw)); len(fields) > 0 {
		v, _ := strconv.ParseFloat(fields[0], 64)
		return v
	}
	return 0
}

func readMem() (total, used uint64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total >= avail {
		used = total - avail
	}
	return total, used
}

func readDisk(path string) (total, used uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	used = (st.Blocks - st.Bavail) * bs
	return total, used
}

// readNet sums rx/tx bytes across all non-loopback interfaces.
func readNet() (rx, tx uint64) {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}
