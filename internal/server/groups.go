package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"fleetcore/internal/bus"

	"fleetcore/internal/api"
	"fleetcore/internal/store"
)

// ---- Desired-state resolution ----
//
// A machine's effective DesiredState is computed, never edited directly:
//
//	union of Modules from every group the machine belongs to
//	  (membership = explicit member OR labels match group selector;
//	   empty selector = explicit members only)
//	then the machine's Override specs replace same-named modules.
//
// Group conflicts on the same module name resolve deterministically:
// groups are applied in name order, later names win. Output modules are
// sorted by name so revision bumps only on real change.

func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func computeModules(m api.Machine, groups []api.Group, members map[string]map[string]bool) []api.ModuleSpec {
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	eff := map[string]api.ModuleSpec{}
	for _, g := range groups {
		if !g.MatchAll && !members[g.ID][m.ID] && !selectorMatches(g.Selector, m.Labels) {
			continue
		}
		for _, spec := range g.Modules {
			eff[spec.Name] = spec
		}
	}
	for _, spec := range m.Override {
		eff[spec.Name] = spec
	}
	out := make([]api.ModuleSpec, 0, len(eff))
	for _, spec := range eff {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolver recomputes machines' effective desired state. Shared by the
// admin API (group/label/override changes) and the agent API (a machine
// enrolling after its groups exist must get computed state immediately).
type Resolver struct {
	Store store.Store
	Bus   bus.Bus
	Log   *slog.Logger
}

// Recompute recalculates desired state for the given machines (all
// machines of the tenant when machineIDs is empty), bumping the revision
// and waking streams only where the module set actually changed.
func (s *Resolver) Recompute(tenantID string, machineIDs ...string) error {
	groups, err := s.Store.ListGroups(tenantID)
	if err != nil {
		return err
	}
	members := map[string]map[string]bool{}
	for _, g := range groups {
		ids, err := s.Store.ListGroupMembers(g.ID)
		if err != nil {
			return err
		}
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		members[g.ID] = set
	}

	var machines []api.Machine
	if len(machineIDs) == 0 {
		if machines, err = s.Store.ListMachines(tenantID); err != nil {
			return err
		}
	} else {
		for _, id := range machineIDs {
			m, err := s.Store.GetMachine(id)
			if err != nil {
				return err
			}
			machines = append(machines, m)
		}
	}

	for _, m := range machines {
		next := computeModules(m, groups, members)
		prev, _ := json.Marshal(m.Desired.Modules)
		cur, _ := json.Marshal(next)
		if string(prev) == string(cur) {
			continue
		}
		m.Desired = api.DesiredState{Revision: m.Desired.Revision + 1, Modules: next}
		if err := s.Store.SaveMachine(m); err != nil {
			return err
		}
		s.Bus.Publish(m.ID)
		s.Log.Info("desired state recomputed", "machine", m.ID, "revision", m.Desired.Revision, "modules", len(next))
	}
	return nil
}

// validateModules checks every referenced module artifact exists.
func (s *AdminServer) validateModules(w http.ResponseWriter, specs []api.ModuleSpec) bool {
	for _, spec := range specs {
		if _, err := s.Store.GetModule(spec.Name, spec.Version); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "unknown module "+spec.Name+"@"+spec.Version, http.StatusBadRequest)
				return false
			}
			http.Error(w, "store error", http.StatusInternalServerError)
			return false
		}
	}
	return true
}

// ---- Group handlers ----

func (s *AdminServer) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string            `json:"tenant_id"`
		Name     string            `json:"name"`
		MatchAll bool              `json:"match_all"`
		Selector map[string]string `json:"selector"`
		Modules  []api.ModuleSpec  `json:"modules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TenantID == "" || req.Name == "" {
		http.Error(w, "tenant_id and name required", http.StatusBadRequest)
		return
	}
	if _, err := s.Store.GetTenant(req.TenantID); err != nil {
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}
	if !s.validateModules(w, req.Modules) {
		return
	}
	g := api.Group{
		ID: newID(), TenantID: req.TenantID, Name: req.Name, MatchAll: req.MatchAll,
		Selector: req.Selector, Modules: req.Modules, Created: time.Now().Unix(),
	}
	if err := s.Store.SaveGroup(g); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("group created", "group", g.ID, "name", g.Name)
	if err := s.Resolver.Recompute(g.TenantID); err != nil {
		s.Log.Error("recompute after group create", "err", err)
	}
	writeJSON(w, g)
}

func (s *AdminServer) listGroups(w http.ResponseWriter, r *http.Request) {
	gs, err := s.Store.ListGroups(r.URL.Query().Get("tenant"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, gs)
}

func (s *AdminServer) updateGroup(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGroup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Name     *string            `json:"name"`
		Selector *map[string]string `json:"selector"`
		Modules  *[]api.ModuleSpec  `json:"modules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name != nil {
		g.Name = *req.Name
	}
	if req.Selector != nil {
		g.Selector = *req.Selector
	}
	if req.Modules != nil {
		if !s.validateModules(w, *req.Modules) {
			return
		}
		g.Modules = *req.Modules
	}
	if err := s.Store.SaveGroup(g); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(g.TenantID); err != nil {
		s.Log.Error("recompute after group update", "err", err)
	}
	writeJSON(w, g)
}

func (s *AdminServer) deleteGroup(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGroup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.Store.DeleteGroup(g.ID); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("group deleted", "group", g.ID, "name", g.Name)
	if err := s.Resolver.Recompute(g.TenantID); err != nil {
		s.Log.Error("recompute after group delete", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AdminServer) addGroupMember(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGroup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	var req struct {
		MachineID string `json:"machine_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MachineID == "" {
		http.Error(w, "machine_id required", http.StatusBadRequest)
		return
	}
	m, err := s.Store.GetMachine(req.MachineID)
	if err != nil || m.TenantID != g.TenantID {
		http.Error(w, "unknown machine in this tenant", http.StatusNotFound)
		return
	}
	if err := s.Store.AddGroupMember(g.ID, m.ID); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(g.TenantID, m.ID); err != nil {
		s.Log.Error("recompute after member add", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AdminServer) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGroup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	mid := r.PathValue("machine_id")
	if err := s.Store.RemoveGroupMember(g.ID, mid); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(g.TenantID, mid); err != nil {
		s.Log.Error("recompute after member remove", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AdminServer) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGroup(r.PathValue("id"))
	if err != nil {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}
	explicit, err := s.Store.ListGroupMembers(g.ID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	explicitSet := map[string]bool{}
	for _, id := range explicit {
		explicitSet[id] = true
	}
	machines, err := s.Store.ListMachines(g.TenantID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	type member struct {
		MachineID string `json:"machine_id"`
		Name      string `json:"name"`
		Via       string `json:"via"` // explicit | selector | both
	}
	out := []member{}
	for _, m := range machines {
		exp, sel := explicitSet[m.ID], selectorMatches(g.Selector, m.Labels)
		if !g.MatchAll && !exp && !sel {
			continue
		}
		via := "explicit"
		switch {
		case g.MatchAll:
			via = "all"
		case exp && sel:
			via = "both"
		case sel:
			via = "selector"
		}
		out = append(out, member{MachineID: m.ID, Name: m.Name, Via: via})
	}
	writeJSON(w, out)
}

// putLabels replaces a machine's labels and recomputes its state.
func (s *AdminServer) putLabels(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMachine(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m.Labels = req.Labels
	if err := s.Store.SaveMachine(m); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(m.TenantID, m.ID); err != nil {
		s.Log.Error("recompute after labels", "err", err)
	}
	writeJSON(w, map[string]any{"machine_id": m.ID, "labels": m.Labels})
}
