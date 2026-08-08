package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Modules run as child processes and cannot present the agent's mTLS
// client certificate. To let a collector module (e.g. the GPU collector)
// push metrics, the agent runs a localhost-only HTTP listener that
// accepts a JSON body on stdin-via-helper and forwards it over the
// authenticated connection to /v1/metrics. FLEET_LOCAL_PUSH points the
// module at a tiny helper script that pipes stdin to this listener.

var (
	pushOnce sync.Once
	pushAddr string
	pushErr  error
)

// startLocalPush lazily starts the localhost forwarder and returns its
// address. Bound to 127.0.0.1 with an ephemeral port; only local
// processes (the agent's own module children) can reach it.
func (a *Agent) startLocalPush() (string, error) {
	pushOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			pushErr = err
			return
		}
		pushAddr = ln.Addr().String()
		mux := http.NewServeMux()
		mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Forward over the authenticated agent connection.
			if err := a.postRaw(context.Background(), "/v1/metrics", body); err != nil {
				a.Log.Warn("local push forward failed", "err", err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		go http.Serve(ln, mux)
		a.Log.Info("local module push listener started", "addr", pushAddr)
	})
	return pushAddr, pushErr
}

// localPushHelper writes (once) a small helper script that pipes stdin to
// the local push listener, and returns its path for FLEET_LOCAL_PUSH.
func (a *Agent) localPushHelper() string {
	addr, err := a.startLocalPush()
	if err != nil {
		return "" // module sees empty; collector modules will error clearly
	}
	path := filepath.Join(a.DataDir, "push-helper.sh")
	script := "#!/bin/sh\nexec curl -s -X POST --data-binary @- http://" + addr + "/push\n"
	_ = os.WriteFile(path, []byte(script), 0o700)
	return path
}

// postRaw sends a pre-marshaled JSON body to the control plane.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func (a *Agent) postRaw(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ServerURL+path, bytesReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A rejected push must surface: reporting success on a 4xx made
	// collector-module failures invisible on both sides of the wire.
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
