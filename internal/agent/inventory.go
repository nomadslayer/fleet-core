package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"fleetcore/internal/api"
)

// CollectInventory gathers host facts from /proc and /etc only — no
// subprocesses, so it costs microseconds and works on any distro.
func CollectInventory() api.Inventory {
	inv := api.Inventory{
		Arch:         runtime.GOARCH,
		AgentVersion: Version,
	}
	inv.Hostname, _ = os.Hostname()

	if id, ver := osRelease(); id != "" {
		inv.OS, inv.OSVersion = id, ver
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		inv.Kernel = strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if up, err := strconv.ParseFloat(fields[0], 64); err == nil {
				inv.UptimeSec = int64(up)
			}
		}
	}
	inv.CPUCores = runtime.NumCPU()
	inv.Packages = packageCount()
	inv.Processes, inv.Services = scanProcs()
	inv.GPUs = detectGPUs()
	inv.Kubernetes = detectKubernetes(inv.Services)
	inv.Interfaces, inv.PrimaryIP = collectInterfaces()
	return inv
}

func k3sRole(cmdlinePath string) string {
	raw, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return ""
	}
	return k3sRoleFromCmdline(raw)
}

// k3sRoleFromCmdline classifies a k3s process from /proc/<pid>/cmdline,
// returning "" when the process is not k3s at all.
//
// The kernel NUL-separates argv, but k3s re-execs itself and rewrites its
// process title, so the server process shows up as the single argument
// "k3s server" — space-separated inside one element. Splitting on NUL
// alone therefore found no subcommand and every k3s node was reported
// with an unknown role. Split on both.
func k3sRoleFromCmdline(raw []byte) string {
	fields := strings.FieldsFunc(string(raw), func(r rune) bool {
		return r == 0 || r == ' ' || r == '\t' || r == '\n'
	})
	if len(fields) == 0 || !strings.Contains(filepath.Base(fields[0]), "k3s") {
		return ""
	}
	for _, f := range fields[1:] {
		switch f {
		case "server":
			return "k3s-server"
		case "agent":
			return "k3s-agent"
		}
	}
	// The k3s binary without a recognised subcommand — the container
	// entrypoint's "init" supervisor, for instance. Still a K8s node.
	return "k3s"
}

// detectKubernetes derives a machine's Kubernetes role from the set of
// running services (no kubectl, no API calls). Reported as a short tag
// so groups/dashboards can select on it: "control-plane", "worker",
// "k3s-server", "k3s-agent", or "" when not a K8s node.
func detectKubernetes(services []api.Service) string {
	has := map[string]bool{}
	for _, s := range services {
		has[s.Name] = true
	}
	switch {
	case has["k3s-server"]:
		return "k3s-server"
	case has["k3s-agent"]:
		return "k3s-agent"
	case has["k3s"]:
		return "k3s" // role could not be read from argv, but it is a K8s node
	case has["kube-apiserver"]:
		return "control-plane"
	case has["kubelet"]:
		return "worker"
	default:
		return ""
	}
}

// pciGPUVendors maps PCI vendor IDs (display-class devices) to names.
var pciGPUVendors = map[string]string{
	"0x10de": "nvidia",
	"0x1002": "amd",
	"0x8086": "intel",
}

// detectGPUs enumerates display-class PCI devices via sysfs; for NVIDIA,
// the driver procfs supplies the exact model name. No subprocesses
// (nvidia-smi etc.), so this stays cheap and dependency-free.
func detectGPUs() []string {
	var gpus []string
	entries, err := os.ReadDir("/sys/bus/pci/devices")
	if err != nil {
		return nil
	}
	nvidiaModels := nvidiaModelNames()
	nvIdx := 0
	for _, e := range entries {
		base := "/sys/bus/pci/devices/" + e.Name()
		class, err := os.ReadFile(base + "/class")
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(class)), "0x03") {
			continue // 0x03xxxx = display controller
		}
		vendorRaw, _ := os.ReadFile(base + "/vendor")
		vendor := pciGPUVendors[strings.TrimSpace(string(vendorRaw))]
		if vendor == "" {
			vendor = strings.TrimSpace(string(vendorRaw))
		}
		if vendor == "nvidia" && nvIdx < len(nvidiaModels) {
			gpus = append(gpus, "nvidia: "+nvidiaModels[nvIdx])
			nvIdx++
			continue
		}
		device, _ := os.ReadFile(base + "/device")
		gpus = append(gpus, vendor+": device "+strings.TrimSpace(string(device)))
	}
	return gpus
}

func nvidiaModelNames() []string {
	dirs, err := os.ReadDir("/proc/driver/nvidia/gpus")
	if err != nil {
		return nil
	}
	var models []string
	for _, d := range dirs {
		raw, err := os.ReadFile("/proc/driver/nvidia/gpus/" + d.Name() + "/information")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "Model:"); ok {
				models = append(models, strings.TrimSpace(v))
			}
		}
	}
	return models
}

// knownServices maps /proc/<pid>/comm values to a canonical service name.
// detectedService maps a /proc/<pid>/comm value to a canonical name and
// a category, so reporting can distinguish databases from orchestration
// from web servers rather than lumping everything into one list.
type detectedService struct {
	Name     string
	Category string // database | kubernetes | container | web | messaging | ai | monitoring | system
}

var knownServices = map[string]detectedService{
	// databases
	"postgres":        {"postgresql", "database"},
	"mysqld":          {"mysql", "database"},
	"mariadbd":        {"mariadb", "database"},
	"mongod":          {"mongodb", "database"},
	"redis-server":    {"redis", "database"},
	"valkey-server":   {"valkey", "database"},
	"clickhouse-serv": {"clickhouse", "database"},
	"memcached":       {"memcached", "database"},
	"sqld":            {"sqld", "database"},
	"etcd":            {"etcd", "database"},
	// kubernetes control/data plane
	"kubelet":         {"kubelet", "kubernetes"},
	"kube-apiserver":  {"kube-apiserver", "kubernetes"},
	"kube-scheduler":  {"kube-scheduler", "kubernetes"},
	"kube-controller": {"kube-controller-manager", "kubernetes"},
	"kube-proxy":      {"kube-proxy", "kubernetes"},
	// k3s: comm is "k3s" regardless of role; k3sRole reads argv to split
	// server from agent. The two role names below are what it produces.
	"k3s":        {"k3s", "kubernetes"},
	"k3s-server": {"k3s-server", "kubernetes"},
	"k3s-agent":  {"k3s-agent", "kubernetes"},
	"k0s":        {"k0s", "kubernetes"},
	// container runtimes
	"dockerd":    {"docker", "container"},
	"containerd": {"containerd", "container"},
	"crio":       {"crio", "container"},
	// web / proxy
	"nginx":   {"nginx", "web"},
	"haproxy": {"haproxy", "web"},
	"caddy":   {"caddy", "web"},
	// messaging
	"nats-server": {"nats", "messaging"},
	// ai / inference
	"ollama": {"ollama", "ai"},
	// monitoring
	"prometheus":    {"prometheus", "monitoring"},
	"node_exporter": {"node_exporter", "monitoring"},
	// system
	"sshd": {"sshd", "system"},
}

// scanProcs walks /proc once, returning the process count and the
// recognised server software found running, grouped by category.
func scanProcs() (int, []api.Service) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, nil
	}
	count := 0
	found := map[string]detectedService{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		count++
		raw, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(raw))
		// k3s ships one binary for both roles and re-execs itself, so it
		// shows up as comm "k3s" (argv "k3s server") or comm "exe" (re-exec
		// via /proc/self/exe, argv[0] still k3s). Neither survives a plain
		// comm lookup, which is why k3s nodes reported no Kubernetes role.
		if comm == "k3s" || comm == "exe" {
			if role := k3sRole("/proc/" + e.Name() + "/cmdline"); role != "" {
				found[role] = detectedService{Name: role, Category: "kubernetes"}
				continue
			}
		}
		if svc, ok := knownServices[comm]; ok {
			found[svc.Name] = svc
		}
	}
	services := make([]api.Service, 0, len(found))
	for _, svc := range found {
		services = append(services, api.Service{Name: svc.Name, Category: svc.Category})
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].Category != services[j].Category {
			return services[i].Category < services[j].Category
		}
		return services[i].Name < services[j].Name
	})
	return count, services
}

func osRelease() (id, version string) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var pretty string
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "ID="); ok {
			id = strings.Trim(v, `"`)
		}
		if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
			version = strings.Trim(v, `"`)
		}
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			pretty = strings.Trim(v, `"`)
		}
	}
	// ID and VERSION_ID are optional in the os-release spec, and minimal
	// images (K3s, some container bases) ship PRETTY_NAME alone — which
	// left the OS column blank. Fall back to splitting PRETTY_NAME.
	if id == "" && pretty != "" {
		if name, ver, ok := strings.Cut(pretty, " "); ok {
			id, version = strings.ToLower(name), ver
		} else {
			id = strings.ToLower(pretty)
		}
	}
	return id, version
}

// packageCount is best-effort: dpkg status entries or rpm db presence.
func packageCount() int {
	if f, err := os.Open("/var/lib/dpkg/status"); err == nil {
		defer f.Close()
		n := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "Package:") {
				n++
			}
		}
		return n
	}
	// rpm-based distros: counting needs librpm; report -1 = "unknown"
	if _, err := os.Stat("/var/lib/rpm"); err == nil {
		return -1
	}
	return 0
}
