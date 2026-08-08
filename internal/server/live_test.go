package server

import (
	"strings"
	"testing"
	"time"

	"fleetcore/internal/api"
)

// TestGPUMergeAndGauges verifies a GPU-only sample merges into an
// existing core sample and both surface as Prometheus gauges.
func TestLiveGaugesIncludeGPUandProcs(t *testing.T) {
	live := NewLiveRegistry()
	now := time.Now().Unix()
	live.Set("m1", api.MetricsSample{
		AtUnix:     now,
		CPUPercent: 42,
		MemUsed:    1 << 30, MemTotal: 4 << 30,
		DiskUsed: 1 << 30, DiskTotal: 10 << 30,
		TopProcesses: []api.ProcSample{
			{PID: 111, Comm: "python", CPUPercent: 30.5, MemBytes: 512 << 20},
			{PID: 222, Comm: "postgres", CPUPercent: 8.0, MemBytes: 256 << 20},
		},
		GPUs: []api.GPUSample{
			{Index: 0, Name: "NVIDIA A100", UtilPercent: 87, MemUsed: 40 << 30, MemTotal: 80 << 30, TempC: 72, PowerW: 340},
		},
	})

	admin := &AdminServer{Live: live}
	var b strings.Builder
	machines := []api.Machine{{ID: "m1", Name: "gpu-node"}}
	admin.writeLiveMetrics(&b, machines)
	out := b.String()

	checks := []string{
		`fleet_live_cpu_percent{machine_id="m1",name="gpu-node"} 42`,
		`fleet_live_process_cpu_percent{machine_id="m1",name="gpu-node",pid="111",comm="python"} 30.5`,
		`fleet_live_process_memory_bytes{machine_id="m1",name="gpu-node",pid="222",comm="postgres"}`,
		`fleet_live_gpu_utilization_percent{machine_id="m1",name="gpu-node",index="0",gpu="NVIDIA A100"} 87`,
		`fleet_live_gpu_temp_celsius{machine_id="m1",name="gpu-node",index="0",gpu="NVIDIA A100"} 72`,
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("missing gauge line:\n  %s", c)
		}
	}
}

// TestStaleGPUExcluded ensures a stale sample is not emitted.
func TestStaleSampleExcluded(t *testing.T) {
	live := NewLiveRegistry()
	live.Set("old", api.MetricsSample{AtUnix: time.Now().Add(-2 * time.Minute).Unix(), CPUPercent: 99})
	admin := &AdminServer{Live: live}
	var b strings.Builder
	admin.writeLiveMetrics(&b, []api.Machine{{ID: "old", Name: "stale"}})
	if strings.Contains(b.String(), `name="stale"`) {
		t.Error("stale machine should be excluded from live gauges")
	}
}
