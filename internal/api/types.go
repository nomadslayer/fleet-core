// Package api defines the wire types shared by the agent, the control
// plane and the admin API. Keep this dependency-free: it is the contract.
package api

// ---- Enrollment ----

type EnrollRequest struct {
	Token  string `json:"token"`   // one-time enrollment token (tenant scoped)
	CSRPEM string `json:"csr_pem"` // PKCS#10 CSR; only the public key is used
	Name   string `json:"name"`    // optional human-readable machine name
}

type EnrollResponse struct {
	MachineID    string `json:"machine_id"`
	TenantID     string `json:"tenant_id"`
	CertPEM      string `json:"cert_pem"`       // client certificate (mTLS identity)
	CAPEM        string `json:"ca_pem"`         // root CA to pin for all future calls
	ModulePubPEM string `json:"module_pub_pem"` // ed25519 public key that signs module payloads
}

// ---- Inventory / heartbeat ----

type Inventory struct {
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`         // e.g. "ubuntu"
	OSVersion    string    `json:"os_version"` // e.g. "24.04"
	Kernel       string    `json:"kernel"`
	Arch         string    `json:"arch"`
	CPUCores     int       `json:"cpu_cores,omitempty"`
	UptimeSec    int64     `json:"uptime_sec"`
	Packages     int       `json:"packages"`             // installed package count (best effort)
	Processes    int       `json:"processes"`            // running process count
	GPUs         []string  `json:"gpus,omitempty"`       // e.g. ["nvidia: NVIDIA A100-SXM4-80GB"]
	Services     []Service `json:"services,omitempty"`   // detected server software, categorized
	Kubernetes   string    `json:"kubernetes,omitempty"` // "" | control-plane | worker | k3s-server | k3s-agent
	Updates      *Updates  `json:"updates,omitempty"`    // pending package updates (nil until first scan)
	AgentVersion string    `json:"agent_version"`

	// Interfaces lists every network interface. Machines routinely have
	// several (physical, bond, bridge, VPN, container veth), so there is no
	// single "the IP" — PrimaryIP names the address on whichever interface
	// carries the default route, which is what an operator usually means.
	Interfaces []NetInterface `json:"interfaces,omitempty"`
	PrimaryIP  string         `json:"primary_ip,omitempty"`
}

// NetInterface is one network interface with its addresses.
type NetInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	MTU       int      `json:"mtu,omitempty"`
	SpeedMbps int      `json:"speed_mbps,omitempty"` // 0 when unknown (virtual interfaces)
	Up        bool     `json:"up"`
	Primary   bool     `json:"primary,omitempty"` // carries the default route
	Virtual   bool     `json:"virtual,omitempty"` // veth/bridge/docker/cni — noise on K8s nodes
	IPv4      []string `json:"ipv4,omitempty"`    // CIDR form, e.g. "10.0.0.5/24"
	IPv6      []string `json:"ipv6,omitempty"`
}

// Service is a recognised piece of server software found running, tagged
// with a category so consumers distinguish databases from Kubernetes
// components from web servers rather than reading a flat list.
type Service struct {
	Name     string `json:"name"`
	Category string `json:"category"` // database | kubernetes | container | web | messaging | ai | monitoring | system
}

// Updates summarises pending package updates from the host package
// manager. Counts are cheap to collect and safe to report on every
// heartbeat; the security count is best-effort (apt only, currently).
type Updates struct {
	Manager   string   `json:"manager"`            // apt | dnf | unknown
	Total     int      `json:"total"`              // upgradable packages
	Security  int      `json:"security"`           // subset flagged security (apt: -security suite)
	Packages  []string `json:"packages,omitempty"` // up to 50 names, for detail views
	RebootReq bool     `json:"reboot_required"`    // /var/run/reboot-required (Debian)
	AtUnix    int64    `json:"at_unix"`
}

type Heartbeat struct {
	Inventory Inventory `json:"inventory"`
}

// MetricsSample is a point-in-time live reading pushed by the agent.
// The control plane keeps only the latest sample per machine in memory;
// durable time series belong to Prometheus via scrape or a collector
// module pushing remote_write.
type MetricsSample struct {
	AtUnix     int64   `json:"at_unix"`
	CPUPercent float64 `json:"cpu_percent"`
	Load1      float64 `json:"load1"`
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	DiskTotal  uint64  `json:"disk_total"` // root filesystem
	DiskUsed   uint64  `json:"disk_used"`
	NetRxBps   float64 `json:"net_rx_bps"`
	NetTxBps   float64 `json:"net_tx_bps"`

	TopProcesses []ProcSample `json:"top_processes,omitempty"` // htop-style, top N by CPU
	GPUs         []GPUSample  `json:"gpus,omitempty"`          // per-GPU utilization (from a collector)

	// Process totals cover the whole process table, not just the top-N
	// above: a top-N list alone cannot answer "how much memory are
	// processes actually using".
	ProcCount    int    `json:"proc_count,omitempty"`
	ProcTotalRSS uint64 `json:"proc_total_rss,omitempty"` // summed RSS across all processes

	// Interfaces carries per-interface throughput. NetRxBps/NetTxBps above
	// remain the non-loopback total so existing consumers keep working.
	Interfaces []NetSample `json:"interfaces,omitempty"`

	// Pods is per-pod resource usage on a Kubernetes node.
	Pods []PodSample `json:"pods,omitempty"`
}

// ProcSample is one process in the per-process breakdown.
type ProcSample struct {
	PID        int     `json:"pid"`
	Comm       string  `json:"comm"`              // process name (/proc/<pid>/comm, 15 chars max)
	CPUPercent float64 `json:"cpu_percent"`       // over the last sample interval
	MemBytes   uint64  `json:"mem_bytes"`         // resident set size
	Service    string  `json:"service,omitempty"` // canonical name when this is recognised server software
}

// NetSample is one interface's throughput over the last interval.
type NetSample struct {
	Name    string  `json:"name"`
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
	RxBytes uint64  `json:"rx_bytes"` // cumulative since boot
	TxBytes uint64  `json:"tx_bytes"`
}

// PodSample is one Kubernetes pod's resource usage. Collected from the
// kubelet Summary API when reachable, otherwise derived from the pod
// cgroup hierarchy (which yields usage but only a UID, no names).
type PodSample struct {
	Namespace  string            `json:"namespace,omitempty"`
	Name       string            `json:"name,omitempty"`
	UID        string            `json:"uid,omitempty"`
	CPUPercent float64           `json:"cpu_percent"`
	MemBytes   uint64            `json:"mem_bytes"`
	Source     string            `json:"source,omitempty"` // kubelet | cgroup
	Containers []ContainerSample `json:"containers,omitempty"`
}

// ContainerSample is one container inside a pod.
type ContainerSample struct {
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	MemBytes   uint64  `json:"mem_bytes"`
}

// GPUSample is one GPU's utilization at a point in time. Populated by a
// collector module pushing to /v1/metrics/gpu; the agent itself has no
// NVML dependency.
type GPUSample struct {
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	UtilPercent float64 `json:"util_percent"`
	MemUsed     uint64  `json:"mem_used"`
	MemTotal    uint64  `json:"mem_total"`
	TempC       float64 `json:"temp_c"`
	PowerW      float64 `json:"power_w"`
}

// ---- Desired state / modules ----

// ModuleSpec is what the operator wants running/applied on a machine.
type ModuleSpec struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Config  map[string]string `json:"config,omitempty"` // passed to module as FLEET_CFG_* env
}

// DesiredState is the full declarative target for one machine.
// Agents reconcile toward it; they are never imperatively commanded.
type DesiredState struct {
	Revision int64        `json:"revision"`
	Modules  []ModuleSpec `json:"modules"`
}

// ModuleArtifact is a signed, versioned payload stored on the control plane.
type ModuleArtifact struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Exec      string `json:"exec"`      // entrypoint relative to payload dir, default "payload"
	Payload   []byte `json:"payload"`   // opaque bytes (script, binary, tarball)
	Signature []byte `json:"signature"` // ed25519 over payload
}

type ModuleStatus struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	State   string `json:"state"` // applied | failed
	Detail  string `json:"detail,omitempty"`
	AtUnix  int64  `json:"at_unix"`
}

type StatusReport struct {
	Revision int64          `json:"revision"`
	Modules  []ModuleStatus `json:"modules"`
}

// ---- Control-plane records ----

type Tenant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

type EnrollToken struct {
	Token     string            `json:"token"`
	TenantID  string            `json:"tenant_id"`
	Labels    map[string]string `json:"labels,omitempty"` // stamped onto the machine at enrollment
	Created   int64             `json:"created"`
	ExpiresAt int64             `json:"expires_at"` // unix; 0 = no expiry
	MaxUses   int               `json:"max_uses"`   // 0 or 1 = single-use; -1 = unlimited
	Uses      int               `json:"uses"`
	Used      bool              `json:"used"` // true once exhausted
}

type Machine struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Enrolled  int64             `json:"enrolled"`
	LastSeen  int64             `json:"last_seen"`
	Inventory Inventory         `json:"inventory"`
	// Override holds machine-specific module specs; they win over group
	// modules of the same name during resolution.
	Override []ModuleSpec `json:"override,omitempty"`
	// Desired is the COMPUTED state the agent reconciles toward:
	// union of matching groups' modules, then Override applied on top.
	Desired DesiredState   `json:"desired"`
	Status  []ModuleStatus `json:"status"`
}

// Group clusters machines for management. Membership is the union of
// explicit members and machines whose labels match Selector (all pairs
// must match; empty selector = explicit members only). Modules listed
// here become part of every member's computed desired state.
type Group struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	// MatchAll makes every machine in the tenant a member, regardless of
	// labels or explicit membership. Each tenant gets a built-in "all"
	// group with this set — the place for baseline modules.
	MatchAll bool              `json:"match_all,omitempty"`
	Selector map[string]string `json:"selector,omitempty"`
	Modules  []ModuleSpec      `json:"modules,omitempty"`
	Created  int64             `json:"created"`
}
