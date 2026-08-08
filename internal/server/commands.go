package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"fleetcore/internal/api"
)

// Ad-hoc commands.
//
// A "command" is not a new primitive: it is a module with a generated
// name, so it inherits everything modules already have — ed25519 signing,
// pull-based delivery over the reconcile stream, once-per-(name,version)
// execution, and captured output reported back as module status. Nothing
// is pushed to a machine; the agent pulls when it sees its desired state
// change, which is what keeps NAT'd machines reachable.
//
// Because each command gets a unique name, history and queue fall out of
// state that already exists rather than needing a second store:
//
//	in desired state + reported in status  -> executed (applied | failed)
//	in desired state + absent from status  -> queued, not yet run
//
// Removing a command from desired state removes its history too, which is
// why cmdPrefix commands are left in place rather than pruned on success.
const cmdPrefix = "cmd-"

// defaultCommandHistory bounds how many ad-hoc commands are retained per
// target. Commands live in desired state so that their results stay
// visible, but desired state is sent to the agent on every reconcile and
// each command is also a stored artifact — left unbounded, a long-lived
// machine would accumulate an ever-growing reconcile payload, an
// ever-growing applied-state file, and one stored payload per command
// forever. Oldest are dropped once the cap is exceeded. Override with
// --max-command-history.
const defaultCommandHistory = 20

// maxCommands is the configured retention, falling back to the default so
// a zero value never means "keep nothing".
func (s *AdminServer) maxCommands() int {
	if s.MaxCommandHistory > 0 {
		return s.MaxCommandHistory
	}
	return defaultCommandHistory
}

// pruneCommands trims cmd-* specs to the newest maxCommandHistory,
// returning the kept specs and the names that were dropped. Ordering is by
// the creation timestamp encoded in the name, so it does not depend on the
// slice order the store happened to return.
func pruneCommands(specs []api.ModuleSpec, limit int) (kept []api.ModuleSpec, dropped []string) {
	var cmds []api.ModuleSpec
	for _, sp := range specs {
		if isCommand(sp.Name) {
			cmds = append(cmds, sp)
		}
	}
	if limit <= 0 {
		limit = defaultCommandHistory
	}
	if len(cmds) <= limit {
		return specs, nil
	}
	sort.Slice(cmds, func(i, j int) bool {
		return commandCreatedAt(cmds[i].Name) > commandCreatedAt(cmds[j].Name)
	})
	remove := map[string]bool{}
	for _, sp := range cmds[limit:] {
		remove[sp.Name] = true
		dropped = append(dropped, sp.Name)
	}
	for _, sp := range specs {
		if !remove[sp.Name] {
			kept = append(kept, sp)
		}
	}
	return kept, dropped
}

// forgetCommands deletes the stored payloads of pruned commands so the
// module table does not grow without bound either.
func (s *AdminServer) forgetCommands(names []string) {
	for _, n := range names {
		if err := s.Store.DeleteModule(n, "1"); err != nil {
			s.Log.Warn("could not delete pruned command", "command", n, "err", err)
		}
	}
	if len(names) > 0 {
		s.Log.Info("pruned command history", "dropped", len(names), "keep", s.maxCommands())
	}
}

func nowUnix() int64 { return time.Now().Unix() }

// machineGroups answers "which groups is this machine in", which the group
// records alone cannot: membership is the union of match-all, selector
// matches, and an explicit membership list held separately. Without this
// the UI could not show a machine's groups at all.
func (s *AdminServer) machineGroups(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMachine(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	groups, err := s.Store.ListGroups(m.TenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	type row struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Via  string `json:"via"` // match_all | selector | explicit
		// Selector is echoed back for selector membership so the UI can show
		// *which* labels matched rather than just asserting that some did.
		Selector map[string]string `json:"selector,omitempty"`
		Modules  int               `json:"modules"`
	}
	out := []row{}
	for _, g := range groups {
		via := ""
		switch {
		case g.MatchAll:
			via = "match_all"
		case selectorMatches(g.Selector, m.Labels) && len(g.Selector) > 0:
			via = "selector"
		default:
			members, err := s.Store.ListGroupMembers(g.ID)
			if err != nil {
				continue
			}
			for _, id := range members {
				if id == m.ID {
					via = "explicit"
					break
				}
			}
		}
		if via == "" {
			continue
		}
		r := row{ID: g.ID, Name: g.Name, Via: via, Modules: len(g.Modules)}
		if via == "selector" {
			r.Selector = g.Selector
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

// commandName generates a unique, sortable module name. The timestamp is
// embedded so the UI can order and date commands without another table.
func (s *AdminServer) commandName(now int64) string {
	return fmt.Sprintf("%s%d-%s", cmdPrefix, now, newID()[:6])
}

// commandCreatedAt recovers the creation time encoded in the name.
func commandCreatedAt(name string) int64 {
	rest, ok := strings.CutPrefix(name, cmdPrefix)
	if !ok {
		return 0
	}
	ts, _, _ := strings.Cut(rest, "-")
	v, _ := strconv.ParseInt(ts, 10, 64)
	return v
}

func isCommand(name string) bool { return strings.HasPrefix(name, cmdPrefix) }

// createCommand wraps a script as a signed module and adds it to the
// desired state of either a whole group or one machine.
func (s *AdminServer) createCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Script     string            `json:"script"`
		TargetKind string            `json:"target_kind"` // group | machine
		TargetID   string            `json:"target_id"`
		Selector   map[string]string `json:"selector,omitempty"` // target_kind=selector
		Label      string            `json:"label,omitempty"` // free-text, shown in the UI
		Config     map[string]string `json:"config,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Script = strings.TrimSpace(req.Script)
	if req.Script == "" {
		http.Error(w, "script required", http.StatusBadRequest)
		return
	}
	switch req.TargetKind {
	case "group", "machine":
		if req.TargetID == "" {
			http.Error(w, "target_id required", http.StatusBadRequest)
			return
		}
	case "selector":
		// A selector spans groups: it matches on labels directly, so
		// "role=gpu AND region=hk" needs no group to exist for that
		// intersection. target_id carries the tenant to scope the search.
		if len(req.Selector) == 0 {
			http.Error(w, "selector required", http.StatusBadRequest)
			return
		}
		if req.TargetID == "" {
			http.Error(w, "target_id must be the tenant id for selector targeting", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "target_kind must be group, machine or selector", http.StatusBadRequest)
		return
	}
	// The payload is executed directly, so it needs an interpreter line.
	// Accepting a bare command list and adding one is the difference between
	// "type a command" and "write a script".
	// The agent runs a module with its working directory set to that
	// module's unpack dir, which is right for modules (they may ship files
	// beside the payload) but surprising for an ad-hoc command: "ls" would
	// list the command's own payload. Anchor commands at / instead.
	script := req.Script
	if !strings.HasPrefix(script, "#!") {
		script = "#!/bin/sh\nset -eu\ncd /\n" + script + "\n"
	}

	now := nowUnix()
	name := s.commandName(now)
	art := api.ModuleArtifact{
		Name:      name,
		Version:   "1",
		Exec:      "payload",
		Payload:   []byte(script),
		Signature: s.CA.SignModule([]byte(script)),
	}
	if err := s.Store.SaveModule(art); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	spec := api.ModuleSpec{Name: name, Version: "1", Config: req.Config}
	if req.Label != "" {
		if spec.Config == nil {
			spec.Config = map[string]string{}
		}
		spec.Config["label"] = req.Label
	}

	matched := 1
	switch req.TargetKind {
	case "group":
		g, err := s.Store.GetGroup(req.TargetID)
		if err != nil {
			http.Error(w, "unknown group", http.StatusNotFound)
			return
		}
		g.Modules = append(g.Modules, spec)
		var dropped []string
		g.Modules, dropped = pruneCommands(g.Modules, s.maxCommands())
		if err := s.Store.SaveGroup(g); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		s.forgetCommands(dropped)
		if err := s.Resolver.Recompute(g.TenantID); err != nil {
			s.Log.Error("recompute after command", "err", err)
		}
		s.Log.Info("command queued", "command", name, "group", g.Name, "tenant", g.TenantID)
	case "machine":
		m, err := s.Store.GetMachine(req.TargetID)
		if err != nil {
			http.Error(w, "unknown machine", http.StatusNotFound)
			return
		}
		m.Override = append(m.Override, spec)
		var dropped []string
		m.Override, dropped = pruneCommands(m.Override, s.maxCommands())
		if err := s.Store.SaveMachine(m); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		s.forgetCommands(dropped)
		if err := s.Resolver.Recompute(m.TenantID, m.ID); err != nil {
			s.Log.Error("recompute after command", "err", err)
		}
		s.Log.Info("command queued", "command", name, "machine", m.Name)

	case "selector":
		machines, err := s.Store.ListMachines(req.TargetID)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		var hit []string
		for _, m := range machines {
			if !selectorMatches(req.Selector, m.Labels) {
				continue
			}
			m.Override = append(m.Override, spec)
			var dropped []string
			m.Override, dropped = pruneCommands(m.Override, s.maxCommands())
			if err := s.Store.SaveMachine(m); err != nil {
				s.Log.Error("save machine for selector command", "machine", m.ID, "err", err)
				continue
			}
			s.forgetCommands(dropped)
			hit = append(hit, m.ID)
		}
		if len(hit) == 0 {
			// Nothing matched: drop the artifact rather than leaving an
			// orphan payload that no machine will ever pull.
			_ = s.Store.DeleteModule(name, "1")
			http.Error(w, "selector matched no machines", http.StatusNotFound)
			return
		}
		if err := s.Resolver.Recompute(req.TargetID, hit...); err != nil {
			s.Log.Error("recompute after command", "err", err)
		}
		matched = len(hit)
		s.Log.Info("command queued", "command", name, "selector", req.Selector, "machines", len(hit))
	}

	writeJSON(w, map[string]any{
		"name": name, "version": "1", "created": now,
		"target_kind": req.TargetKind, "target_id": req.TargetID,
		"selector": req.Selector, "matched": matched,
		"script": script,
	})
}

// listCommands returns every ad-hoc command with its script, newest
// first. The UI joins these against a machine's desired/status by name.
func (s *AdminServer) listCommands(w http.ResponseWriter, r *http.Request) {
	mods, err := s.Store.ListModules()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	type cmd struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Created int64  `json:"created"`
		Script  string `json:"script"`
	}
	// The retention cap is surfaced so the UI can say what it is rather
	// than duplicating the number.
	w.Header().Set("X-Fleet-Command-History", strconv.Itoa(s.maxCommands()))
	out := []cmd{}
	for _, m := range mods {
		if !isCommand(m.Name) {
			continue
		}
		out = append(out, cmd{Name: m.Name, Version: m.Version, Created: commandCreatedAt(m.Name)})
	}
	// Newest first, and cap how many scripts are fetched. ListModules omits
	// payloads, so each script costs one store round-trip; pulling every
	// one made this endpoint O(commands) round-trips and slow to open.
	// Clients cache scripts by name, so only the recent page is needed.
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		if full, err := s.Store.GetModule(out[i].Name, out[i].Version); err == nil {
			out[i].Script = string(full.Payload)
		}
	}
	writeJSON(w, out)
}

// cancelCommand removes a queued command from a machine's override or a
// group, so an operator can clear something that has not run yet (or drop
// a finished one from the history view).
func (s *AdminServer) cancelCommand(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !isCommand(name) {
		http.Error(w, "not a command", http.StatusBadRequest)
		return
	}
	kind, id := r.URL.Query().Get("target_kind"), r.URL.Query().Get("target_id")
	drop := func(specs []api.ModuleSpec) []api.ModuleSpec {
		out := specs[:0]
		for _, sp := range specs {
			if sp.Name != name {
				out = append(out, sp)
			}
		}
		return out
	}
	switch kind {
	case "group":
		g, err := s.Store.GetGroup(id)
		if err != nil {
			http.Error(w, "unknown group", http.StatusNotFound)
			return
		}
		g.Modules = drop(g.Modules)
		if err := s.Store.SaveGroup(g); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		_ = s.Resolver.Recompute(g.TenantID)
	case "machine":
		m, err := s.Store.GetMachine(id)
		if err != nil {
			http.Error(w, "unknown machine", http.StatusNotFound)
			return
		}
		m.Override = drop(m.Override)
		if err := s.Store.SaveMachine(m); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		_ = s.Resolver.Recompute(m.TenantID, m.ID)
	default:
		http.Error(w, "target_kind must be group or machine", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
