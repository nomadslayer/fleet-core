package agent

import (
	"testing"

	"fleetcore/internal/api"
)

func svcs(names ...string) []api.Service {
	out := make([]api.Service, len(names))
	for i, n := range names {
		out[i] = api.Service{Name: n}
	}
	return out
}

func TestDetectKubernetes(t *testing.T) {
	cases := []struct {
		name string
		in   []api.Service
		want string
	}{
		{"plain node", svcs("nginx", "docker"), ""},
		{"worker", svcs("kubelet", "containerd"), "worker"},
		{"control-plane", svcs("kubelet", "kube-apiserver", "etcd"), "control-plane"},
		{"k3s server", svcs("k3s-server"), "k3s-server"},
		{"k3s agent", svcs("k3s-agent"), "k3s-agent"},
		// k3s reports comm "k3s" for both roles; if k3sRole cannot read argv
		// the bare name must still not be mistaken for a plain node.
		{"k3s unclassified", svcs("k3s"), "k3s"},
		{"apiserver wins", svcs("kubelet", "kube-apiserver"), "control-plane"},
	}
	for _, c := range cases {
		if got := detectKubernetes(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestK3sRoleFromCmdline(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		// Normal NUL-separated argv, as systemd starts it.
		{"nul separated server", "/usr/local/bin/k3s\x00server\x00--disable\x00traefik\x00", "k3s-server"},
		{"nul separated agent", "/usr/local/bin/k3s\x00agent\x00", "k3s-agent"},
		// k3s re-execs and rewrites its process title into ONE argument;
		// this is the form that made every k3s node report an unknown role.
		{"rewritten title server", "k3s server\x00\x00\x00", "k3s-server"},
		{"rewritten title agent", "k3s agent\x00", "k3s-agent"},
		// The container entrypoint supervisor: k3s, but no role in argv.
		{"init supervisor", "/bin/k3s\x00init\x00", "k3s"},
		// Anything else must not be claimed as Kubernetes.
		{"not k3s", "/usr/sbin/nginx\x00-g\x00daemon off;\x00", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := k3sRoleFromCmdline([]byte(c.raw)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
