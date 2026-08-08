package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The pod cgroup tree has three shapes in the wild and they look nothing
// alike. Only the cgroupfs one was ever exercised against a live cluster
// (k3s-in-docker), so these fixtures pin the other two — in particular the
// systemd driver that kubeadm defaults to, where the pod UID is escaped
// with underscores and every level is prefixed and .slice-suffixed.
func mkdirs(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindPodCgroupsSystemdDriver(t *testing.T) {
	root := t.TempDir()
	// kubeadm default: systemd cgroup driver. UID dashes become underscores.
	mkdirs(t, root,
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod099b88fb_2a99_45ad_af94_d91eb81a87e9.slice",
		"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod729d2f46_480d_4796_a76c_e227e2671d31.slice",
		// Guaranteed pods hang directly off kubepods.slice with no QoS level.
		"kubepods.slice/kubepods-podd80f4a0e_576f_47a8_a0cf_69dc23dc1980.slice",
		// Container scopes live below the pod; we must report the pod level.
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod099b88fb_2a99_45ad_af94_d91eb81a87e9.slice/cri-containerd-abc123.scope",
		// Unrelated slices must not be picked up.
		"system.slice/sshd.service",
		"user.slice",
	)
	got := findPodCgroups(root)
	want := []string{
		"099b88fb-2a99-45ad-af94-d91eb81a87e9",
		"729d2f46-480d-4796-a76c-e227e2671d31",
		"d80f4a0e-576f-47a8-a0cf-69dc23dc1980",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d pods, want %d: %v", len(got), len(want), got)
	}
	for _, uid := range want {
		dir, ok := got[uid]
		if !ok {
			t.Errorf("missing pod %s (underscores must be normalised to dashes); got %v", uid, got)
			continue
		}
		// Must be the pod slice itself, never the container scope below it.
		if filepath.Ext(dir[0]) != ".slice" {
			t.Errorf("pod %s resolved to %q, want the .slice directory", uid, dir[0])
		}
	}
}

func TestFindPodCgroupsCgroupfsDriver(t *testing.T) {
	root := t.TempDir()
	// k3s / cgroupfs driver: plain nesting, UID keeps its dashes.
	mkdirs(t, root,
		"kubepods/burstable/pod099b88fb-2a99-45ad-af94-d91eb81a87e9",
		"kubepods/besteffort/pod729d2f46-480d-4796-a76c-e227e2671d31",
		"kubepods/podd80f4a0e-576f-47a8-a0cf-69dc23dc1980",
		"kubepods/burstable/pod099b88fb-2a99-45ad-af94-d91eb81a87e9/abc123container",
		"podruntime",
	)
	got := findPodCgroups(root)
	if len(got) != 3 {
		t.Fatalf("found %d pods, want 3: %v", len(got), got)
	}
	if _, ok := got["099b88fb-2a99-45ad-af94-d91eb81a87e9"]; !ok {
		t.Errorf("missing burstable pod: %v", got)
	}
	if _, ok := got["d80f4a0e-576f-47a8-a0cf-69dc23dc1980"]; !ok {
		t.Errorf("missing guaranteed pod directly under kubepods: %v", got)
	}
}

// cgroup v1 splits by controller, so the kubepods tree sits under
// /sys/fs/cgroup/<controller>/ rather than at the root.
func TestFindPodCgroupsV1ControllerLayout(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root,
		"memory/kubepods/burstable/pod099b88fb-2a99-45ad-af94-d91eb81a87e9",
		"cpuacct/kubepods/burstable/pod099b88fb-2a99-45ad-af94-d91eb81a87e9",
		"blkio/kubepods/besteffort/pod729d2f46-480d-4796-a76c-e227e2671d31",
	)
	got := findPodCgroups(root)
	if len(got) != 2 {
		t.Fatalf("found %d pods, want 2 (deduped across controllers): %v", len(got), got)
	}
}

func TestFindPodCgroupsNonKubernetesHost(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "system.slice/nginx.service", "user.slice/user-1000.slice", "init.scope")
	if got := findPodCgroups(root); len(got) != 0 {
		t.Errorf("non-Kubernetes host must yield no pods, got %v", got)
	}
}

func TestPodLogNames(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root,
		"kube-system_coredns-ccb96694c-cmv9s_099b88fb-2a99-45ad-af94-d91eb81a87e9",
		"default_web-cbfbbd684-d8r4t_729d2f46-480d-4796-a76c-e227e2671d31",
		// Pod names may themselves contain underscores; only the last field
		// is the UID and only the first is the namespace.
		"myns_my_app_with_underscores_d80f4a0e-576f-47a8-a0cf-69dc23dc1980",
		"not-a-pod-dir",
	)
	got := podLogNamesIn(root)
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3: %v", len(got), got)
	}
	if n := got["099b88fb-2a99-45ad-af94-d91eb81a87e9"]; n.namespace != "kube-system" || n.name != "coredns-ccb96694c-cmv9s" {
		t.Errorf("coredns parsed as %+v", n)
	}
	if n := got["d80f4a0e-576f-47a8-a0cf-69dc23dc1980"]; n.namespace != "myns" || n.name != "my_app_with_underscores" {
		t.Errorf("underscored pod name parsed as %+v", n)
	}
}
