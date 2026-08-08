package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fleetcore/internal/api"
)

// reconcile drives the machine toward the desired module set.
//
// Model (v0.1): modules are one-shot "apply" payloads, executed once per
// (name, version, config) tuple. Long-running supervision belongs to a
// future supervisor module, not the core. Applied state is remembered in
// <data>/applied.json so restarts don't re-run everything.
func (a *Agent) reconcile(ctx context.Context, ds api.DesiredState) {
	a.Log.Info("reconciling", "revision", ds.Revision, "modules", len(ds.Modules))
	applied := a.loadApplied()
	var statuses []api.ModuleStatus

	want := make(map[string]bool, len(ds.Modules))
	for _, spec := range ds.Modules {
		key := appliedKey(spec)
		want[key] = true
		if st, ok := applied[key]; ok && st.State == "applied" {
			statuses = append(statuses, st) // already converged
			continue
		}
		st := a.apply(ctx, spec)
		applied[key] = st
		statuses = append(statuses, st)
	}
	// Forget anything no longer desired. Without this, applied.json and the
	// unpacked module directories grow for the life of the machine — which
	// ad-hoc commands, being one module each, would do quickly.
	for key, st := range applied {
		if want[key] {
			continue
		}
		delete(applied, key)
		a.removeModuleDir(st.Name, st.Version)
	}
	a.saveApplied(applied)

	if err := a.postJSON(ctx, "/v1/status", api.StatusReport{Revision: ds.Revision, Modules: statuses}, nil); err != nil {
		a.Log.Warn("status report failed", "err", err)
	}
}

func (a *Agent) apply(ctx context.Context, spec api.ModuleSpec) api.ModuleStatus {
	st := api.ModuleStatus{Name: spec.Name, Version: spec.Version, AtUnix: time.Now().Unix()}
	fail := func(err error) api.ModuleStatus {
		a.Log.Error("module failed", "module", spec.Name, "version", spec.Version, "err", err)
		st.State, st.Detail = "failed", err.Error()
		return st
	}

	var art api.ModuleArtifact
	q := url.Values{"name": {spec.Name}, "version": {spec.Version}}
	if err := a.getJSON(ctx, "/v1/module?"+q.Encode(), &art); err != nil {
		return fail(fmt.Errorf("download: %w", err))
	}
	if err := a.verify(art); err != nil {
		return fail(fmt.Errorf("signature: %w", err))
	}

	dir := filepath.Join(a.DataDir, "modules", spec.Name+"-"+spec.Version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	execName := filepath.Base(filepath.Clean(art.Exec)) // no path escape
	binPath := filepath.Join(dir, execName)
	if err := os.WriteFile(binPath, art.Payload, 0o700); err != nil {
		return fail(err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, binPath)
	cmd.Dir = dir
	cmd.Env = append(minimalEnv(),
		"FLEET_MODULE_NAME="+spec.Name,
		"FLEET_MODULE_VERSION="+spec.Version,
		"FLEET_MACHINE_ID="+a.machineID,
		"FLEET_LOCAL_PUSH="+a.localPushHelper(),
	)
	for k, v := range spec.Config {
		cmd.Env = append(cmd.Env, "FLEET_CFG_"+strings.ToUpper(k)+"="+v)
	}
	out, err := cmd.CombinedOutput()
	detail := truncate(string(out), 4096)
	if err != nil {
		st.State = "failed"
		st.Detail = fmt.Sprintf("%v: %s", err, detail)
		a.Log.Error("module failed", "module", spec.Name, "version", spec.Version, "err", err)
		return st
	}
	st.State, st.Detail = "applied", detail
	a.Log.Info("module applied", "module", spec.Name, "version", spec.Version)
	return st
}

// verify checks the ed25519 signature against the key pinned at enrollment.
func (a *Agent) verify(art api.ModuleArtifact) error {
	block, _ := pem.Decode(a.modKey)
	if block == nil {
		return errors.New("no pinned module key")
	}
	anyKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	pub, ok := anyKey.(ed25519.PublicKey)
	if !ok {
		return errors.New("pinned module key is not ed25519")
	}
	if !ed25519.Verify(pub, art.Payload, art.Signature) {
		return errors.New("payload signature invalid")
	}
	return nil
}

// ---- applied-state bookkeeping ----

func appliedKey(spec api.ModuleSpec) string {
	keys := make([]string, 0, len(spec.Config))
	for k := range spec.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(spec.Name + "@" + spec.Version)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + spec.Config[k])
	}
	return b.String()
}

func (a *Agent) appliedPath() string { return filepath.Join(a.DataDir, "applied.json") }

// removeModuleDir deletes a module's unpacked payload. Best effort: a
// failure here only wastes disk, never correctness.
func (a *Agent) removeModuleDir(name, version string) {
	if name == "" {
		return
	}
	dir := filepath.Join(a.DataDir, "modules", name+"-"+version)
	if err := os.RemoveAll(dir); err != nil {
		a.Log.Warn("could not remove module dir", "module", name, "err", err)
	}
}

func (a *Agent) loadApplied() map[string]api.ModuleStatus {
	out := map[string]api.ModuleStatus{}
	raw, err := os.ReadFile(a.appliedPath())
	if err == nil {
		_ = jsonUnmarshal(raw, &out)
	}
	return out
}

func (a *Agent) saveApplied(m map[string]api.ModuleStatus) {
	raw, err := jsonMarshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(a.appliedPath(), raw, 0o600)
}

func minimalEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
