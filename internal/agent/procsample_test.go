package agent

import (
	"os"
	"testing"

	"fleetcore/internal/api"
)

func TestParseStatReal(t *testing.T) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("no /proc")
	}
	pc, ok := parseStat(string(raw), uint64(os.Getpagesize()))
	if !ok {
		t.Fatal("parseStat failed on /proc/self/stat")
	}
	if pc.comm == "" {
		t.Error("empty comm")
	}
	if pc.rss == 0 {
		t.Error("zero RSS for self (should have resident memory)")
	}
	t.Logf("self: comm=%q jiffies=%d rss=%d bytes", pc.comm, pc.jiff, pc.rss)
}

func TestParseStatCommWithSpaces(t *testing.T) {
	// comm can contain spaces and parens; ensure we split on last ')'
	fake := "1234 (weird (proc) name) S 1 1234 1234 0 -1 4194304 100 0 0 0 " +
		"42 17 0 0 20 0 1 0 999 12345678 256 " +
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0"
	pc, ok := parseStat(fake, 4096)
	if !ok {
		t.Fatal("parse failed")
	}
	if pc.comm != "weird (proc) name" {
		t.Errorf("comm = %q, want %q", pc.comm, "weird (proc) name")
	}
	if pc.jiff != 42+17 {
		t.Errorf("jiffies = %d, want 59", pc.jiff)
	}
	if pc.rss != 256*4096 {
		t.Errorf("rss = %d, want %d", pc.rss, 256*4096)
	}
}

func TestProcessReportDelta(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc") // Linux-only; keeps the suite green on macOS
	}
	prev := map[int]procCPU{}
	// first call primes prev: no per-process CPU yet, but totals are real
	first := processReport(prev, 1000, 4, 5)
	if len(first.Samples) != 0 {
		t.Errorf("first call should return no samples, got %v", first.Samples)
	}
	if first.Count == 0 || first.TotalRSS == 0 {
		t.Errorf("totals should be populated on the first call: count=%d rss=%d", first.Count, first.TotalRSS)
	}
	if len(prev) == 0 {
		t.Error("prev should be populated after first call")
	}
	// second call computes deltas
	second := processReport(prev, 1000, 4, 5)
	if second.Count == 0 {
		t.Error("process count should be non-zero")
	}
	// The selection unions top-N by CPU, top-N by RSS and all recognised
	// services, so it can exceed n — but not without bound.
	if len(second.Samples) > 2*5+len(knownServices) {
		t.Errorf("returned %d samples, implausibly many", len(second.Samples))
	}
}

func TestSelectProcessesUnionsRankings(t *testing.T) {
	all := []api.ProcSample{
		{PID: 1, Comm: "busy", CPUPercent: 90, MemBytes: 1},
		{PID: 2, Comm: "hog", CPUPercent: 0, MemBytes: 1 << 40}, // idle memory hog
		{PID: 3, Comm: "kubelet", CPUPercent: 0.1, MemBytes: 2, Service: "kubelet"},
		{PID: 4, Comm: "noise", CPUPercent: 0.2, MemBytes: 3},
	}
	got := selectProcesses(all, 1)
	have := map[int]bool{}
	for _, p := range got {
		have[p.PID] = true
	}
	if !have[1] {
		t.Error("top CPU process must be included")
	}
	if !have[2] {
		t.Error("top memory process must be included even at 0% CPU")
	}
	if !have[3] {
		t.Error("recognised services must be included regardless of rank")
	}
}
