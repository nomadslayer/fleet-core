package agent

import "testing"

// TestServiceCategories asserts every known service has a sane category
// and that the category set is what consumers expect to filter on.
func TestServiceCategories(t *testing.T) {
	valid := map[string]bool{
		"database": true, "kubernetes": true, "container": true,
		"web": true, "messaging": true, "ai": true, "monitoring": true, "system": true,
	}
	want := map[string]string{
		"postgres": "database", "mongod": "database", "redis-server": "database",
		"kubelet": "kubernetes", "kube-apiserver": "kubernetes", "k3s-server": "kubernetes",
		"dockerd": "container", "containerd": "container", "crio": "container",
		"nginx": "web", "haproxy": "web",
		"nats-server": "messaging", "ollama": "ai",
		"prometheus": "monitoring", "sshd": "system",
	}
	for comm, cat := range want {
		svc, ok := knownServices[comm]
		if !ok {
			t.Errorf("%s not in knownServices", comm)
			continue
		}
		if svc.Category != cat {
			t.Errorf("%s: category %q, want %q", comm, svc.Category, cat)
		}
	}
	for comm, svc := range knownServices {
		if !valid[svc.Category] {
			t.Errorf("%s has invalid category %q", comm, svc.Category)
		}
	}
}
