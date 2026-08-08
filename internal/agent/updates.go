package agent

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"fleetcore/internal/api"
)

// Update detection is the one inventory area that needs subprocesses:
// package databases have no stable on-disk format to parse safely across
// distros, so we shell out to the package manager's read-only query.
// It's expensive (apt can take seconds), so it runs on its own slow
// cadence and the result is cached; heartbeats attach the last snapshot.

type updateCache struct {
	snap *api.Updates
	at   time.Time
}

// updatesSnapshot returns the cached update summary, refreshing it if
// older than the given max age. Safe to call from the heartbeat path.
func (a *Agent) updatesSnapshot(ctx context.Context, maxAge time.Duration) *api.Updates {
	a.mu.Lock()
	cached := a.updates.snap
	fresh := a.updates.snap != nil && time.Since(a.updates.at) < maxAge
	a.mu.Unlock()
	if fresh {
		return cached
	}
	snap := scanUpdates(ctx)
	a.mu.Lock()
	a.updates = updateCache{snap: snap, at: time.Now()}
	a.mu.Unlock()
	return snap
}

func scanUpdates(ctx context.Context) *api.Updates {
	switch {
	case hasBin("apt-get"):
		return scanApt(ctx)
	case hasBin("dnf"):
		return scanDnf(ctx)
	default:
		return &api.Updates{Manager: "unknown", AtUnix: time.Now().Unix()}
	}
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// scanApt lists upgradable packages without touching the network
// (relies on the apt cache already refreshed by the system's timer or a
// patch module run). Security updates are counted by origin suite.
func scanApt(ctx context.Context) *api.Updates {
	u := &api.Updates{Manager: "apt", AtUnix: time.Now().Unix()}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "apt-get", "-s", "upgrade")
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return u
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		u.Total++
		if len(u.Packages) < 50 {
			u.Packages = append(u.Packages, fields[1])
		}
		if strings.Contains(strings.ToLower(line), "security") {
			u.Security++
		}
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		u.RebootReq = true
	}
	return u
}

func scanDnf(ctx context.Context) *api.Updates {
	u := &api.Updates{Manager: "dnf", AtUnix: time.Now().Unix()}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// check-update exits 100 when updates exist, 0 when none.
	cmd := exec.CommandContext(cctx, "dnf", "--cacheonly", "check-update")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, _ := cmd.Output()
	sc := bufio.NewScanner(bytes.NewReader(out))
	inObsolete := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Obsoleting") {
			inObsolete = true
			continue
		}
		if inObsolete {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		u.Total++
		if len(u.Packages) < 50 {
			u.Packages = append(u.Packages, fields[0])
		}
	}
	// dnf security counting needs the updateinfo plugin; count separately.
	sec := exec.CommandContext(cctx, "dnf", "--cacheonly", "updateinfo", "list", "security")
	sec.Env = append(os.Environ(), "LC_ALL=C")
	if secOut, err := sec.Output(); err == nil {
		s := bufio.NewScanner(bytes.NewReader(secOut))
		for s.Scan() {
			if strings.Contains(strings.ToLower(s.Text()), "security") {
				u.Security++
			}
		}
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		u.RebootReq = true
	}
	return u
}
