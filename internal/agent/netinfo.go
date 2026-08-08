package agent

import (
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"fleetcore/internal/api"
)

// Network interface inventory. A machine rarely has exactly one NIC —
// bare metal bonds two, cloud VMs add a management interface, and a
// Kubernetes node grows a veth per pod. So the agent reports every
// interface with its addresses, and separately names the one carrying
// the default route as primary: that is the address an operator means
// when they ask "what is this machine's IP".

// virtualPrefixes are interface-name prefixes that are almost always
// machine-internal plumbing rather than a real network attachment. They
// are still reported (a bridge IP can matter) but flagged so the UI can
// keep a 200-pod node readable.
var virtualPrefixes = []string{
	"lo", "veth", "docker", "br-", "cni", "flannel", "cali", "cilium",
	"kube-ipvs", "nodelocaldns", "virbr", "vxlan", "tunl", "dummy", "ifb",
}

func isVirtualIface(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// defaultRouteIface returns the interface carrying the IPv4 default
// route, read from /proc/net/route (destination 00000000). Empty when
// there is no default route or /proc is unavailable.
func defaultRouteIface() string {
	raw, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	best, bestMetric := "", -1
	for i, line := range strings.Split(string(raw), "\n") {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" {
			continue
		}
		metric, _ := strconv.Atoi(f[6])
		// Lowest metric wins, matching the kernel's own selection.
		if best == "" || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	return best
}

// ifaceSpeed reads link speed in Mbit/s from sysfs. Virtual interfaces
// and down links report -1 or error; both come back as 0 (unknown).
func ifaceSpeed(name string) int {
	raw, err := os.ReadFile("/sys/class/net/" + name + "/speed")
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// collectInterfaces enumerates interfaces and their addresses, marking
// the default-route interface as primary. It returns the list plus the
// bare primary IPv4 address for at-a-glance display.
func collectInterfaces() ([]api.NetInterface, string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, ""
	}
	def := defaultRouteIface()

	out := make([]api.NetInterface, 0, len(ifaces))
	primaryIP := ""
	for _, ifc := range ifaces {
		ni := api.NetInterface{
			Name:      ifc.Name,
			MTU:       ifc.MTU,
			Up:        ifc.Flags&net.FlagUp != 0,
			Primary:   ifc.Name == def,
			Virtual:   isVirtualIface(ifc.Name),
			SpeedMbps: ifaceSpeed(ifc.Name),
		}
		if ifc.HardwareAddr != nil {
			ni.MAC = ifc.HardwareAddr.String()
		}
		addrs, err := ifc.Addrs()
		if err == nil {
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if v4 := ipnet.IP.To4(); v4 != nil {
					ni.IPv4 = append(ni.IPv4, ipnet.String())
					if ni.Primary && primaryIP == "" {
						primaryIP = v4.String()
					}
				} else if !ipnet.IP.IsLinkLocalUnicast() {
					ni.IPv6 = append(ni.IPv6, ipnet.String())
				}
			}
		}
		out = append(out, ni)
	}

	// No default route (isolated host, container netns): fall back to the
	// first routable IPv4 on a non-virtual, up interface so the UI still
	// shows something meaningful rather than blank.
	if primaryIP == "" {
		for i := range out {
			ni := &out[i]
			if !ni.Up || ni.Virtual || len(ni.IPv4) == 0 {
				continue
			}
			if ip, _, err := net.ParseCIDR(ni.IPv4[0]); err == nil && !ip.IsLoopback() {
				primaryIP = ip.String()
				ni.Primary = true
				break
			}
		}
	}

	// Primary first, then real interfaces, then virtual plumbing.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		if out[i].Virtual != out[j].Virtual {
			return out[j].Virtual
		}
		return out[i].Name < out[j].Name
	})
	return out, primaryIP
}

// ---- per-interface throughput ----

// ifaceCounters is one interface's cumulative byte counters.
type ifaceCounters struct{ rx, tx uint64 }

// readNetPerIface parses /proc/net/dev into per-interface cumulative
// counters, loopback excluded (it is never interesting and its volume
// swamps real traffic on busy hosts).
func readNetPerIface() map[string]ifaceCounters {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil
	}
	out := map[string]ifaceCounters{}
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || name == "" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[name] = ifaceCounters{rx: rx, tx: tx}
	}
	return out
}

// netSamples turns two counter snapshots into per-interface rates.
// Interfaces that appeared since the previous tick are skipped for one
// interval rather than reported as a huge spike; counter wrap or a
// device reset shows as zero instead of a negative-turned-enormous rate.
func netSamples(prev, cur map[string]ifaceCounters, elapsed float64) []api.NetSample {
	if elapsed <= 0 {
		return nil
	}
	out := make([]api.NetSample, 0, len(cur))
	for name, c := range cur {
		p, ok := prev[name]
		if !ok {
			continue
		}
		var rxBps, txBps float64
		if c.rx >= p.rx {
			rxBps = float64(c.rx-p.rx) / elapsed
		}
		if c.tx >= p.tx {
			txBps = float64(c.tx-p.tx) / elapsed
		}
		out = append(out, api.NetSample{
			Name: name, RxBps: rxBps, TxBps: txBps, RxBytes: c.rx, TxBytes: c.tx,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RxBps+out[i].TxBps != out[j].RxBps+out[j].TxBps {
			return out[i].RxBps+out[i].TxBps > out[j].RxBps+out[j].TxBps
		}
		return out[i].Name < out[j].Name
	})
	return out
}
