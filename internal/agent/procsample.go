package agent

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"fleetcore/internal/api"
)

// procCPU is one process's cumulative CPU jiffies at a sample instant.
type procCPU struct {
	comm string
	jiff uint64 // utime + stime
	rss  uint64 // bytes
}

// procReport is the per-process picture for one interval: a selected set
// of processes plus totals across the whole table, so consumers can see
// both "what is heavy" and "how much is in use overall".
type procReport struct {
	Samples  []api.ProcSample
	TotalRSS uint64 // summed RSS across every process, not just Samples
	Count    int
}

// processReport samples /proc over the interval between the previous
// snapshot and now. It mutates prev in place to hold the current snapshot
// for the next call. The first call (prev empty) returns totals but no
// per-process CPU — a delta needs two readings.
//
// All from /proc, no subprocesses. CPU% is per-process jiffy delta over
// total system jiffy delta, times the CPU count, matching htop's model.
func processReport(prev map[int]procCPU, totalDelta uint64, ncpu int, n int) procReport {
	var rep procReport
	cur := map[int]procCPU{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return rep
	}
	pageSize := uint64(os.Getpagesize())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		pc, ok := parseStat(string(raw), pageSize)
		if !ok {
			continue
		}
		cur[pid] = pc
	}

	var samples []api.ProcSample
	rep.Count = len(cur)
	for pid, c := range cur {
		rep.TotalRSS += c.rss
		p, ok := prev[pid]
		if !ok {
			continue // new process; a delta needs two readings
		}
		if c.jiff < p.jiff {
			continue // pid reused
		}
		var cpu float64
		if totalDelta > 0 {
			cpu = float64(c.jiff-p.jiff) / float64(totalDelta) * float64(ncpu) * 100
		}
		if cpu <= 0 && c.rss == 0 {
			continue
		}
		ps := api.ProcSample{PID: pid, Comm: c.comm, CPUPercent: cpu, MemBytes: c.rss}
		if svc, ok := knownServices[c.comm]; ok {
			ps.Service = svc.Name
		}
		// A re-exec'd k3s reports comm "exe", which is both unrecognisable
		// and, on a control-plane node, usually the largest process on the
		// box. Resolve it the same way the inventory scan does.
		if c.comm == "exe" || c.comm == "k3s" {
			if role := k3sRole("/proc/" + strconv.Itoa(pid) + "/cmdline"); role != "" {
				ps.Service = role
			}
		}
		samples = append(samples, ps)
	}
	// swap prev <- cur
	for k := range prev {
		delete(prev, k)
	}
	for k, v := range cur {
		prev[k] = v
	}

	rep.Samples = selectProcesses(samples, n)
	return rep
}

// selectProcesses picks what to report from the full process list. A
// straight top-N by CPU is not enough for two common questions:
//
//	"where is my memory going?" — a 40 GiB process idling at 0% CPU never
//	enters a CPU-ranked list, so the union includes the top N by RSS.
//	"what is kubelet using?"    — recognised server software is always
//	included regardless of rank, because its absence from the list is
//	indistinguishable from it not running.
func selectProcesses(all []api.ProcSample, n int) []api.ProcSample {
	if n <= 0 {
		n = 5
	}
	keep := map[int]bool{}
	take := func(sorted []api.ProcSample) {
		for i := 0; i < len(sorted) && i < n; i++ {
			keep[sorted[i].PID] = true
		}
	}

	byCPU := append([]api.ProcSample(nil), all...)
	sort.Slice(byCPU, func(i, j int) bool {
		if byCPU[i].CPUPercent != byCPU[j].CPUPercent {
			return byCPU[i].CPUPercent > byCPU[j].CPUPercent
		}
		return byCPU[i].MemBytes > byCPU[j].MemBytes
	})
	take(byCPU)

	byMem := append([]api.ProcSample(nil), all...)
	sort.Slice(byMem, func(i, j int) bool { return byMem[i].MemBytes > byMem[j].MemBytes })
	take(byMem)

	for _, p := range all {
		if p.Service != "" {
			keep[p.PID] = true
		}
	}

	out := make([]api.ProcSample, 0, len(keep))
	for _, p := range byCPU { // byCPU is already the order we want to emit
		if keep[p.PID] {
			out = append(out, p)
		}
	}
	return out
}

// parseStat extracts comm, utime+stime, and RSS from /proc/pid/stat.
// The comm field is parenthesized and may contain spaces/parens, so we
// split on the last ')' rather than by whitespace.
func parseStat(stat string, pageSize uint64) (procCPU, bool) {
	open := strings.IndexByte(stat, '(')
	close := strings.LastIndexByte(stat, ')')
	if open < 0 || close < 0 || close < open {
		return procCPU{}, false
	}
	comm := stat[open+1 : close]
	rest := strings.Fields(stat[close+1:])
	// rest[0] = state; fields are 1-indexed from field 3 in the man page.
	// After comm, utime = field 14, stime = 15, rss = 24 (in pages).
	// In `rest` (starting at field 3 = state), indices: utime=11, stime=12, rss=21.
	if len(rest) < 22 {
		return procCPU{}, false
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64)
	stime, _ := strconv.ParseUint(rest[12], 10, 64)
	rssPages, _ := strconv.ParseUint(rest[21], 10, 64)
	return procCPU{comm: comm, jiff: utime + stime, rss: rssPages * pageSize}, true
}
