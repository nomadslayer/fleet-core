package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"fleetcore/internal/api"
)

// LiveRegistry holds the latest metrics sample per machine, in memory
// only. Restart loses at most one interval of data; the durable series
// is Prometheus's job. Per-instance by design — with multiple control
// planes, each holds samples for the machines connected to it (the NATS
// bus upgrade carries the fan-out when a cross-instance view is needed).
type LiveRegistry struct {
	mu      sync.RWMutex
	samples map[string]api.MetricsSample
}

func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{samples: map[string]api.MetricsSample{}}
}

func (l *LiveRegistry) Set(machineID string, s api.MetricsSample) {
	l.mu.Lock()
	l.samples[machineID] = s
	l.mu.Unlock()
}

func (l *LiveRegistry) Get(machineID string) (api.MetricsSample, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.samples[machineID]
	return s, ok
}

const liveFreshness = 30 * time.Second

// ---- agent side: ingest ----

func (s *AgentServer) handleMetricsPush(w http.ResponseWriter, r *http.Request, id identity) {
	var sample api.MetricsSample
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&sample); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A GPU-only push (from the collector module) carries just GPUs and no
	// core stats; merge it into the existing sample rather than replacing.
	if len(sample.GPUs) > 0 && sample.CPUPercent == 0 && sample.MemTotal == 0 {
		if prev, ok := s.Live.Get(id.MachineID); ok {
			prev.GPUs = sample.GPUs
			prev.AtUnix = sample.AtUnix
			s.Live.Set(id.MachineID, prev)
			s.Bus.Publish("live:" + id.MachineID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.Live.Set(id.MachineID, sample)
	s.Bus.Publish("live:" + id.MachineID)
	w.WriteHeader(http.StatusNoContent)
}

// ---- admin side: fleet-wide snapshot ----

// handleLiveAll returns the latest sample for every machine that has a
// fresh one, keyed by machine ID. The dashboard's fleet overview polls
// this so it can show live stats for the whole fleet from one request,
// rather than opening an SSE stream per machine; the per-machine stream
// below stays the drill-down path. Stale machines are simply absent,
// which is what makes a dead machine render as "no live data" rather
// than as a frozen last value.
func (s *AdminServer) handleLiveAll(w http.ResponseWriter, r *http.Request) {
	machines, err := s.Store.ListMachines(r.URL.Query().Get("tenant"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	out := make(map[string]api.MetricsSample, len(machines))
	for _, m := range machines {
		if sample, ok := s.Live.Get(m.ID); ok && now.Sub(time.Unix(sample.AtUnix, 0)) <= liveFreshness {
			out[m.ID] = sample
		}
	}
	writeJSON(w, out)
}

// ---- admin side: SSE live stream per machine ----

func (s *AdminServer) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetMachine(id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// The admin server sets a WriteTimeout to bound slow clients, which
	// would otherwise kill this stream mid-sample once it elapses. Clear
	// the deadline for this connection only, so the rest of the admin API
	// keeps its protection.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.Log.Warn("live stream: cannot clear write deadline; stream will be cut short", "err", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	sig, cancel := s.Bus.Subscribe("live:" + id)
	defer cancel()

	send := func() bool {
		sample, ok := s.Live.Get(id)
		if !ok {
			return true // nothing yet; keep waiting
		}
		raw, _ := json.Marshal(sample)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sig:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---- Prometheus gauges from the live registry ----

// writeLiveMetrics appends fleet_live_* gauges for machines with a
// fresh sample (pushed within liveFreshness).
func (s *AdminServer) writeLiveMetrics(b *strings.Builder, machines []api.Machine) {
	now := time.Now()
	type g struct {
		name, help string
		val        func(api.MetricsSample) float64
	}
	gauges := []g{
		{"fleet_live_cpu_percent", "CPU busy percent over the last sample interval.", func(m api.MetricsSample) float64 { return m.CPUPercent }},
		{"fleet_live_load1", "1-minute load average.", func(m api.MetricsSample) float64 { return m.Load1 }},
		{"fleet_live_memory_used_bytes", "Memory in use (total - available).", func(m api.MetricsSample) float64 { return float64(m.MemUsed) }},
		{"fleet_live_memory_total_bytes", "Total memory.", func(m api.MetricsSample) float64 { return float64(m.MemTotal) }},
		{"fleet_live_disk_used_bytes", "Root filesystem used.", func(m api.MetricsSample) float64 { return float64(m.DiskUsed) }},
		{"fleet_live_disk_total_bytes", "Root filesystem size.", func(m api.MetricsSample) float64 { return float64(m.DiskTotal) }},
		{"fleet_live_net_rx_bytes_per_second", "Receive rate across non-loopback interfaces.", func(m api.MetricsSample) float64 { return m.NetRxBps }},
		{"fleet_live_net_tx_bytes_per_second", "Transmit rate across non-loopback interfaces.", func(m api.MetricsSample) float64 { return m.NetTxBps }},
	}
	for _, gauge := range gauges {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", gauge.name, gauge.help, gauge.name)
		for _, m := range machines {
			sample, ok := s.Live.Get(m.ID)
			if !ok || now.Sub(time.Unix(sample.AtUnix, 0)) > liveFreshness {
				continue
			}
			fmt.Fprintf(b, "%s{machine_id=%q,name=%q} %g\n", gauge.name, esc(m.ID), esc(m.Name), gauge.val(sample))
		}
	}

	// fresh gates every gauge below on the machine having reported within
	// liveFreshness, so a dead machine drops out of the scrape instead of
	// flatlining on its last value.
	fresh := func(m api.Machine) (api.MetricsSample, bool) {
		s2, ok := s.Live.Get(m.ID)
		if !ok || now.Sub(time.Unix(s2.AtUnix, 0)) > liveFreshness {
			return api.MetricsSample{}, false
		}
		return s2, true
	}

	// Process totals across the whole table, so "how much memory are
	// processes using" is answerable without summing a truncated top-N.
	for _, g := range []struct {
		name, help string
		val        func(api.MetricsSample) float64
	}{
		{"fleet_live_process_count", "Total running processes.", func(m api.MetricsSample) float64 { return float64(m.ProcCount) }},
		{"fleet_live_process_rss_total_bytes", "Summed RSS across every process.", func(m api.MetricsSample) float64 { return float64(m.ProcTotalRSS) }},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, m := range machines {
			sample, ok := s.Live.Get(m.ID)
			if !ok || now.Sub(time.Unix(sample.AtUnix, 0)) > liveFreshness {
				continue
			}
			fmt.Fprintf(b, "%s{machine_id=%q,name=%q} %g\n", g.name, esc(m.ID), esc(m.Name), g.val(sample))
		}
	}

	// Per-interface throughput. The aggregate fleet_live_net_* gauges above
	// hide which NIC is carrying the traffic on a multi-homed machine.
	for _, g := range []struct {
		name, help string
		val        func(api.NetSample) float64
	}{
		{"fleet_live_interface_rx_bytes_per_second", "Per-interface receive rate.", func(n api.NetSample) float64 { return n.RxBps }},
		{"fleet_live_interface_tx_bytes_per_second", "Per-interface transmit rate.", func(n api.NetSample) float64 { return n.TxBps }},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, m := range machines {
			if s2, ok := fresh(m); ok {
				for _, n := range s2.Interfaces {
					fmt.Fprintf(b, "%s{machine_id=%q,name=%q,interface=%q} %g\n",
						g.name, esc(m.ID), esc(m.Name), esc(n.Name), g.val(n))
				}
			}
		}
	}

	// Per-pod usage on Kubernetes nodes.
	for _, g := range []struct {
		name, help string
		val        func(api.PodSample) float64
	}{
		{"fleet_live_pod_cpu_percent", "Pod CPU usage as percent of one core.", func(p api.PodSample) float64 { return p.CPUPercent }},
		{"fleet_live_pod_memory_bytes", "Pod working-set memory.", func(p api.PodSample) float64 { return float64(p.MemBytes) }},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, m := range machines {
			if s2, ok := fresh(m); ok {
				for _, p := range s2.Pods {
					fmt.Fprintf(b, "%s{machine_id=%q,name=%q,namespace=%q,pod=%q,uid=%q,source=%q} %g\n",
						g.name, esc(m.ID), esc(m.Name), esc(p.Namespace), esc(p.Name), esc(p.UID), esc(p.Source), g.val(p))
				}
			}
		}
	}
	for _, g := range []struct {
		name, help string
		val        func(api.ContainerSample) float64
	}{
		{"fleet_live_container_cpu_percent", "Container CPU usage as percent of one core.", func(c api.ContainerSample) float64 { return c.CPUPercent }},
		{"fleet_live_container_memory_bytes", "Container working-set memory.", func(c api.ContainerSample) float64 { return float64(c.MemBytes) }},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, m := range machines {
			if s2, ok := fresh(m); ok {
				for _, p := range s2.Pods {
					for _, c := range p.Containers {
						fmt.Fprintf(b, "%s{machine_id=%q,name=%q,namespace=%q,pod=%q,container=%q} %g\n",
							g.name, esc(m.ID), esc(m.Name), esc(p.Namespace), esc(p.Name), esc(c.Name), g.val(c))
					}
				}
			}
		}
	}

	// Per-process gauges (htop-style): only fresh machines, top-N each.
	b.WriteString("# HELP fleet_live_process_cpu_percent Top process CPU% (labeled by pid/comm).\n# TYPE fleet_live_process_cpu_percent gauge\n")
	for _, m := range machines {
		if s2, ok := fresh(m); ok {
			for _, p := range s2.TopProcesses {
				fmt.Fprintf(b, "fleet_live_process_cpu_percent{machine_id=%q,name=%q,pid=\"%d\",comm=%q} %g\n",
					esc(m.ID), esc(m.Name), p.PID, esc(p.Comm), p.CPUPercent)
			}
		}
	}
	b.WriteString("# HELP fleet_live_process_memory_bytes Top process RSS bytes.\n# TYPE fleet_live_process_memory_bytes gauge\n")
	for _, m := range machines {
		if s2, ok := fresh(m); ok {
			for _, p := range s2.TopProcesses {
				fmt.Fprintf(b, "fleet_live_process_memory_bytes{machine_id=%q,name=%q,pid=\"%d\",comm=%q} %d\n",
					esc(m.ID), esc(m.Name), p.PID, esc(p.Comm), p.MemBytes)
			}
		}
	}

	// GPU gauges (from the collector module).
	gpuGauge := func(name, help string, val func(api.GPUSample) float64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, m := range machines {
			if s2, ok := fresh(m); ok {
				for _, g := range s2.GPUs {
					fmt.Fprintf(b, "%s{machine_id=%q,name=%q,index=\"%d\",gpu=%q} %g\n",
						name, esc(m.ID), esc(m.Name), g.Index, esc(g.Name), val(g))
				}
			}
		}
	}
	gpuGauge("fleet_live_gpu_utilization_percent", "GPU utilization percent.", func(g api.GPUSample) float64 { return g.UtilPercent })
	gpuGauge("fleet_live_gpu_memory_used_bytes", "GPU memory used.", func(g api.GPUSample) float64 { return float64(g.MemUsed) })
	gpuGauge("fleet_live_gpu_memory_total_bytes", "GPU memory total.", func(g api.GPUSample) float64 { return float64(g.MemTotal) })
	gpuGauge("fleet_live_gpu_temp_celsius", "GPU temperature.", func(g api.GPUSample) float64 { return g.TempC })
	gpuGauge("fleet_live_gpu_power_watts", "GPU power draw.", func(g api.GPUSample) float64 { return g.PowerW })
}
