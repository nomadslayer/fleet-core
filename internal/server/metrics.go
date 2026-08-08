package server

import (
	"fmt"
	"net/http"
	"strings"

	"fleetcore/internal/api"
)

// handleMetrics exposes fleet-level facts in Prometheus text exposition
// format on the admin listener (GET /metrics, same bearer auth — set
// `authorization.credentials` in the scrape config).
//
// Scope note: these are slow-changing facts carried by heartbeats
// (up/last-seen, inventory, module states). High-frequency host metrics
// (CPU, memory, per-process) are a module concern: run a collector on
// the machine that pushes via remote_write; don't route them through
// the control plane.
func (s *AdminServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	machines, err := s.Store.ListMachines("")
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder

	b.WriteString("# HELP fleet_machine_info Static machine facts (value is always 1).\n# TYPE fleet_machine_info gauge\n")
	for _, m := range machines {
		fmt.Fprintf(&b, "fleet_machine_info{machine_id=%q,name=%q,tenant_id=%q,os=%q,os_version=%q,arch=%q,kernel=%q,agent_version=%q} 1\n",
			esc(m.ID), esc(m.Name), esc(m.TenantID), esc(m.Inventory.OS), esc(m.Inventory.OSVersion),
			esc(m.Inventory.Arch), esc(m.Inventory.Kernel), esc(m.Inventory.AgentVersion))
	}

	gauge := func(name, help string, val func(mch machineRow) (float64, bool)) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, m := range machines {
			if v, ok := val(machineRow{m.ID, m.Name, m}); ok {
				fmt.Fprintf(&b, "%s{machine_id=%q,name=%q} %g\n", name, esc(m.ID), esc(m.Name), v)
			}
		}
	}
	gauge("fleet_machine_last_seen_timestamp_seconds", "Unix time of last heartbeat.",
		func(r machineRow) (float64, bool) { return float64(r.m.LastSeen), true })
	gauge("fleet_machine_uptime_seconds", "Host uptime at last heartbeat.",
		func(r machineRow) (float64, bool) { return float64(r.m.Inventory.UptimeSec), true })
	gauge("fleet_machine_packages", "Installed package count (-1 unknown).",
		func(r machineRow) (float64, bool) { return float64(r.m.Inventory.Packages), true })
	gauge("fleet_machine_processes", "Running process count at last heartbeat.",
		func(r machineRow) (float64, bool) { return float64(r.m.Inventory.Processes), true })
	gauge("fleet_machine_gpus", "Detected GPU count.",
		func(r machineRow) (float64, bool) { return float64(len(r.m.Inventory.GPUs)), true })
	gauge("fleet_machine_desired_revision", "Current desired-state revision.",
		func(r machineRow) (float64, bool) { return float64(r.m.Desired.Revision), true })

	b.WriteString("# HELP fleet_machine_gpu_info Detected GPUs (value is always 1).\n# TYPE fleet_machine_gpu_info gauge\n")
	for _, m := range machines {
		for i, g := range m.Inventory.GPUs {
			fmt.Fprintf(&b, "fleet_machine_gpu_info{machine_id=%q,name=%q,index=\"%d\",gpu=%q} 1\n",
				esc(m.ID), esc(m.Name), i, esc(g))
		}
	}
	b.WriteString("# HELP fleet_machine_service_info Detected server software with category (value is always 1).\n# TYPE fleet_machine_service_info gauge\n")
	for _, m := range machines {
		for _, svc := range m.Inventory.Services {
			fmt.Fprintf(&b, "fleet_machine_service_info{machine_id=%q,name=%q,service=%q,category=%q} 1\n",
				esc(m.ID), esc(m.Name), esc(svc.Name), esc(svc.Category))
		}
	}
	b.WriteString("# HELP fleet_machine_interface_info Network interfaces with addresses (value is always 1).\n# TYPE fleet_machine_interface_info gauge\n")
	for _, m := range machines {
		for _, ifc := range m.Inventory.Interfaces {
			primary, virtual, up := "0", "0", "0"
			if ifc.Primary {
				primary = "1"
			}
			if ifc.Virtual {
				virtual = "1"
			}
			if ifc.Up {
				up = "1"
			}
			fmt.Fprintf(&b, "fleet_machine_interface_info{machine_id=%q,name=%q,interface=%q,mac=%q,ipv4=%q,primary=%q,virtual=%q,up=%q} 1\n",
				esc(m.ID), esc(m.Name), esc(ifc.Name), esc(ifc.MAC), esc(strings.Join(ifc.IPv4, ",")), primary, virtual, up)
		}
	}
	b.WriteString("# HELP fleet_machine_primary_ip_info The machine's default-route IPv4 address (value is always 1).\n# TYPE fleet_machine_primary_ip_info gauge\n")
	for _, m := range machines {
		if m.Inventory.PrimaryIP != "" {
			fmt.Fprintf(&b, "fleet_machine_primary_ip_info{machine_id=%q,name=%q,ip=%q} 1\n",
				esc(m.ID), esc(m.Name), esc(m.Inventory.PrimaryIP))
		}
	}
	b.WriteString("# HELP fleet_machine_kubernetes_info Kubernetes role (value is always 1); absent if not a K8s node.\n# TYPE fleet_machine_kubernetes_info gauge\n")
	for _, m := range machines {
		if m.Inventory.Kubernetes != "" {
			fmt.Fprintf(&b, "fleet_machine_kubernetes_info{machine_id=%q,name=%q,role=%q} 1\n",
				esc(m.ID), esc(m.Name), esc(m.Inventory.Kubernetes))
		}
	}
	b.WriteString("# HELP fleet_machine_updates_pending Upgradable packages at last scan.\n# TYPE fleet_machine_updates_pending gauge\n")
	for _, m := range machines {
		if m.Inventory.Updates != nil {
			fmt.Fprintf(&b, "fleet_machine_updates_pending{machine_id=%q,name=%q,manager=%q} %d\n",
				esc(m.ID), esc(m.Name), esc(m.Inventory.Updates.Manager), m.Inventory.Updates.Total)
		}
	}
	b.WriteString("# HELP fleet_machine_updates_security Security updates pending at last scan.\n# TYPE fleet_machine_updates_security gauge\n")
	for _, m := range machines {
		if m.Inventory.Updates != nil {
			fmt.Fprintf(&b, "fleet_machine_updates_security{machine_id=%q,name=%q} %d\n",
				esc(m.ID), esc(m.Name), m.Inventory.Updates.Security)
		}
	}
	b.WriteString("# HELP fleet_machine_reboot_required 1 if the machine needs a reboot.\n# TYPE fleet_machine_reboot_required gauge\n")
	for _, m := range machines {
		if m.Inventory.Updates != nil {
			v := 0
			if m.Inventory.Updates.RebootReq {
				v = 1
			}
			fmt.Fprintf(&b, "fleet_machine_reboot_required{machine_id=%q,name=%q} %d\n", esc(m.ID), esc(m.Name), v)
		}
	}
	b.WriteString("# HELP fleet_module_applied Module state per machine (1 applied, 0 failed).\n# TYPE fleet_module_applied gauge\n")
	for _, m := range machines {
		for _, st := range m.Status {
			v := 0
			if st.State == "applied" {
				v = 1
			}
			fmt.Fprintf(&b, "fleet_module_applied{machine_id=%q,name=%q,module=%q,version=%q} %d\n",
				esc(m.ID), esc(m.Name), esc(st.Name), esc(st.Version), v)
		}
	}
	s.writeLiveMetrics(&b, machines)
	_, _ = w.Write([]byte(b.String()))
}

type machineRow struct {
	id, name string
	m        api.Machine
}

func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
