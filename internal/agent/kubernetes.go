package agent

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"fleetcore/internal/api"
)

// Per-pod resource usage on a Kubernetes node, collected two ways:
//
//	kubelet — GET /stats/summary on the kubelet (:10250 authenticated, or
//	          :10255 read-only where still enabled). Authoritative: gives
//	          namespace, pod name and per-container numbers straight from
//	          cAdvisor.
//	cgroup  — walk the kubepods cgroup tree. Needs no credentials and no
//	          network, works on every CRI, but the tree is keyed by pod UID
//	          only, so names come from /var/log/pods (the kubelet's own
//	          <namespace>_<name>_<uid> log layout).
//
// The kubelet path is preferred and the cgroup path is the fallback, so a
// node with no token still reports usage rather than nothing.

// podCPU is one pod's cumulative CPU time, for delta computation.
type podCPU struct{ usec uint64 }

// kubeState carries what the collector must remember between ticks.
type kubeState struct {
	prevPods map[string]podCPU // cgroup path -> last CPU reading
	names    map[string]podName
	namesAt  time.Time
	noKubele bool // kubelet probe failed; stop retrying every tick
	client   *http.Client
	token    string
	baseURL  string
}

type podName struct{ namespace, name string }

var podDirRe = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

// collectPods returns per-pod usage, or nil when this is not a K8s node.
func (a *Agent) collectPods(ks *kubeState, elapsed float64) []api.PodSample {
	if ks == nil || elapsed <= 0 {
		return nil
	}
	if pods := a.podsFromKubelet(ks); len(pods) > 0 {
		return pods
	}
	return a.podsFromCgroups(ks, elapsed)
}

// ---- kubelet Summary API ----

// summaryResponse mirrors the subset of the kubelet's /stats/summary
// payload we need. Field names are stable across supported K8s versions.
type summaryResponse struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"podRef"`
		CPU struct {
			UsageNanoCores uint64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			WorkingSetBytes uint64 `json:"workingSetBytes"`
		} `json:"memory"`
		Containers []struct {
			Name string `json:"name"`
			CPU  struct {
				UsageNanoCores uint64 `json:"usageNanoCores"`
			} `json:"cpu"`
			Memory struct {
				WorkingSetBytes uint64 `json:"workingSetBytes"`
			} `json:"memory"`
		} `json:"containers"`
	} `json:"pods"`
}

// kubeletToken finds a bearer token for the kubelet's authenticated port.
// FLEET_KUBELET_TOKEN wins so operators can point at a purpose-made
// ServiceAccount; otherwise the in-pod projected token is used if present.
func kubeletToken() string {
	if t := strings.TrimSpace(os.Getenv("FLEET_KUBELET_TOKEN")); t != "" {
		return t
	}
	if p := os.Getenv("FLEET_KUBELET_TOKEN_FILE"); p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(raw))
		}
	}
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	if raw, err := os.ReadFile(sa); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func (a *Agent) podsFromKubelet(ks *kubeState) []api.PodSample {
	if ks.noKubele {
		return nil
	}
	if ks.client == nil {
		ks.token = kubeletToken()
		// The kubelet serves its own self-signed cert; the connection is to
		// localhost, so skipping verification does not widen exposure the
		// way it would over a network path.
		ks.client = &http.Client{
			Timeout:   3 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}
		ks.baseURL = strings.TrimSpace(os.Getenv("FLEET_KUBELET_URL"))
	}

	urls := []string{}
	if ks.baseURL != "" {
		urls = append(urls, ks.baseURL)
	} else {
		urls = append(urls, "https://127.0.0.1:10250", "http://127.0.0.1:10255")
	}

	for _, base := range urls {
		req, err := http.NewRequest(http.MethodGet, base+"/stats/summary?only_cpu_and_memory=true", nil)
		if err != nil {
			continue
		}
		if ks.token != "" {
			req.Header.Set("Authorization", "Bearer "+ks.token)
		}
		resp, err := ks.client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var sum summaryResponse
		err = json.NewDecoder(resp.Body).Decode(&sum)
		resp.Body.Close()
		if err != nil {
			continue
		}
		out := make([]api.PodSample, 0, len(sum.Pods))
		for _, p := range sum.Pods {
			ps := api.PodSample{
				Namespace: p.PodRef.Namespace, Name: p.PodRef.Name, UID: p.PodRef.UID,
				// nanocores -> percent of one core
				CPUPercent: float64(p.CPU.UsageNanoCores) / 1e9 * 100,
				MemBytes:   p.Memory.WorkingSetBytes,
				Source:     "kubelet",
			}
			for _, c := range p.Containers {
				ps.Containers = append(ps.Containers, api.ContainerSample{
					Name:       c.Name,
					CPUPercent: float64(c.CPU.UsageNanoCores) / 1e9 * 100,
					MemBytes:   c.Memory.WorkingSetBytes,
				})
			}
			out = append(out, ps)
		}
		sortPods(out)
		return out
	}
	// Nothing answered — almost always "not a K8s node" or "no token".
	// Latch it off so we are not probing two ports every tick forever.
	ks.noKubele = true
	a.Log.Info("kubelet stats unavailable; using cgroup fallback for pod usage")
	return nil
}

// ---- cgroup fallback ----

// podLogNames maps pod UID -> namespace/name using the kubelet's log
// directory layout (/var/log/pods/<namespace>_<name>_<uid>). This is the
// only credential-free source of pod identity on a node.
func podLogNames() map[string]podName { return podLogNamesIn("/var/log/pods") }

func podLogNamesIn(root string) map[string]podName {
	out := map[string]podName{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		parts := strings.Split(e.Name(), "_")
		if len(parts) < 3 {
			continue
		}
		uid := parts[len(parts)-1]
		ns := parts[0]
		name := strings.Join(parts[1:len(parts)-1], "_")
		out[uid] = podName{namespace: ns, name: name}
	}
	return out
}

func cgroupV2() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

// readUint pulls the first integer out of a cgroup file.
func readUint(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readKeyed pulls "key value" pairs out of cgroup stat files.
func readKeyed(path, key string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			v, err := strconv.ParseUint(f[1], 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}

// findPodCgroups walks the kubepods hierarchy and returns every directory
// found for each pod UID. Three layouts occur in practice:
//
//	systemd driver (kubeadm default), cgroup v2:
//	  kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice
//	  — with the UID's dashes escaped to underscores
//	cgroupfs driver (k3s and others), cgroup v2:
//	  kubepods/burstable/pod<uid>
//	cgroup v1: the same trees, but once per controller:
//	  <controller>/kubepods/burstable/pod<uid>
//
// A UID maps to a slice of directories because under v1 the CPU and memory
// numbers live in different controller trees, so the caller must be able to
// try each.
func findPodCgroups(root string) map[string][]string {
	out := map[string][]string{}
	// Depth is bounded: kubepods{,.slice}/<qos>/<pod>. Walking the whole of
	// /sys/fs/cgroup on a busy node is needlessly expensive.
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 3 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			full := filepath.Join(dir, name)
			if m := podDirRe.FindStringSubmatch(name); m != nil {
				uid := strings.ReplaceAll(m[1], "_", "-")
				out[uid] = append(out[uid], full)
				continue // containers live below; pod level is what we report
			}
			if strings.Contains(name, "kubepods") || strings.Contains(name, "besteffort") ||
				strings.Contains(name, "burstable") || name == "guaranteed" {
				walk(full, depth+1)
			}
		}
	}

	bases := []string{root, filepath.Join(root, "kubepods.slice"), filepath.Join(root, "kubepods")}
	// cgroup v1 nests everything one level deeper, under a per-controller
	// directory. Probing the root's immediate children covers that without
	// hardcoding the controller names, which vary by kernel config.
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.Contains(e.Name(), "kubepods") {
				continue
			}
			sub := filepath.Join(root, e.Name())
			bases = append(bases, filepath.Join(sub, "kubepods.slice"), filepath.Join(sub, "kubepods"))
		}
	}
	for _, base := range bases {
		if _, err := os.Stat(base); err == nil {
			walk(base, 0)
		}
	}
	return out
}

func (a *Agent) podsFromCgroups(ks *kubeState, elapsed float64) []api.PodSample {
	root := "/sys/fs/cgroup"
	dirs := findPodCgroups(root)
	if len(dirs) == 0 {
		return nil
	}
	// Pod identity changes only when pods churn; re-reading the log dir
	// every tick is wasted syscalls on a node with hundreds of pods.
	if ks.names == nil || time.Since(ks.namesAt) > 30*time.Second {
		ks.names = podLogNames()
		ks.namesAt = time.Now()
	}
	if ks.prevPods == nil {
		ks.prevPods = map[string]podCPU{}
	}
	v2 := cgroupV2()

	cur := map[string]podCPU{}
	out := make([]api.PodSample, 0, len(dirs))
	for uid, candidates := range dirs {
		// Under cgroup v1 a pod appears once per controller and the values
		// are split across them, so every candidate is tried and the first
		// that yields a reading wins.
		firstUint := func(file string) (uint64, bool) {
			for _, d := range candidates {
				if v, ok := readUint(filepath.Join(d, file)); ok {
					return v, true
				}
			}
			return 0, false
		}
		firstKeyed := func(file, key string) (uint64, bool) {
			for _, d := range candidates {
				if v, ok := readKeyed(filepath.Join(d, file), key); ok {
					return v, true
				}
			}
			return 0, false
		}

		var usec, mem uint64
		if v2 {
			if v, ok := firstKeyed("cpu.stat", "usage_usec"); ok {
				usec = v
			}
			if v, ok := firstUint("memory.current"); ok {
				mem = v
				// kubectl top reports the working set, i.e. usage minus
				// reclaimable page cache. Without this a pod that merely read
				// a large file looks like it is hoarding memory.
				if inactive, ok := firstKeyed("memory.stat", "inactive_file"); ok && mem > inactive {
					mem -= inactive
				}
			}
		} else {
			if v, ok := firstUint("cpuacct.usage"); ok {
				usec = v / 1000 // ns -> us
			}
			if v, ok := firstUint("memory.usage_in_bytes"); ok {
				mem = v
				if inactive, ok := firstKeyed("memory.stat", "total_inactive_file"); ok && mem > inactive {
					mem -= inactive
				}
			}
		}
		cur[uid] = podCPU{usec: usec}

		ps := api.PodSample{UID: uid, MemBytes: mem, Source: "cgroup"}
		if n, ok := ks.names[uid]; ok {
			ps.Namespace, ps.Name = n.namespace, n.name
		}
		if p, ok := ks.prevPods[uid]; ok && usec >= p.usec {
			// percent of one core, matching the per-process model
			ps.CPUPercent = float64(usec-p.usec) / 1e6 / elapsed * 100
		}
		out = append(out, ps)
	}
	ks.prevPods = cur
	sortPods(out)
	return out
}

func sortPods(p []api.PodSample) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].CPUPercent != p[j].CPUPercent {
			return p[i].CPUPercent > p[j].CPUPercent
		}
		if p[i].MemBytes != p[j].MemBytes {
			return p[i].MemBytes > p[j].MemBytes
		}
		return p[i].Name < p[j].Name
	})
}
