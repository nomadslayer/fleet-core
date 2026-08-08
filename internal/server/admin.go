package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"fleetcore/internal/api"
	"fleetcore/internal/bus"
	"fleetcore/internal/ca"
	"fleetcore/internal/store"
)

// AdminServer is the operator surface. It is deliberately separate from
// the agent listener so it can be bound to localhost / a private network
// and fronted by whatever authn you standardise on later; the built-in
// guard is a static bearer token.
type AdminServer struct {
	CA       *ca.CA
	Store    store.Store
	Bus      bus.Bus
	Token    string
	Log      *slog.Logger
	Resolver *Resolver
	Live     *LiveRegistry
	// MaxCommandHistory caps retained ad-hoc commands per target; 0 uses
	// defaultCommandHistory.
	MaxCommandHistory int
}

func (s *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("POST /admin/tenants", s.createTenant)
	mux.HandleFunc("GET /admin/tenants", s.listTenants)
	mux.HandleFunc("POST /admin/tokens", s.createToken)
	mux.HandleFunc("GET /admin/machines", s.listMachines)
	mux.HandleFunc("GET /admin/machines/{id}", s.getMachine)
	mux.HandleFunc("PUT /admin/machines/{id}/desired", s.putDesired)
	mux.HandleFunc("POST /admin/groups", s.createGroup)
	mux.HandleFunc("GET /admin/groups", s.listGroups)
	mux.HandleFunc("PATCH /admin/groups/{id}", s.updateGroup)
	mux.HandleFunc("DELETE /admin/groups/{id}", s.deleteGroup)
	mux.HandleFunc("GET /admin/groups/{id}/members", s.listGroupMembers)
	mux.HandleFunc("POST /admin/groups/{id}/members", s.addGroupMember)
	mux.HandleFunc("DELETE /admin/groups/{id}/members/{machine_id}", s.removeGroupMember)
	mux.HandleFunc("PUT /admin/machines/{id}/labels", s.putLabels)
	mux.HandleFunc("DELETE /admin/machines/{id}", s.deleteMachine)
	mux.HandleFunc("POST /admin/modules", s.uploadModule)
	mux.HandleFunc("GET /admin/modules", s.listModules)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /metrics/alerts", s.handleAlertRules)
	mux.HandleFunc("POST /admin/commands", s.createCommand)
	mux.HandleFunc("GET /admin/commands", s.listCommands)
	mux.HandleFunc("DELETE /admin/commands/{name}", s.cancelCommand)
	mux.HandleFunc("GET /admin/machines/{id}/groups", s.machineGroups)
	mux.HandleFunc("GET /admin/live", s.handleLiveAll)
	mux.HandleFunc("GET /admin/machines/{id}/live", s.handleLiveStream)
	return s.auth(mux)
}

func (s *AdminServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/dashboard" {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *AdminServer) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	t := api.Tenant{ID: newID(), Name: req.Name, Created: time.Now().Unix()}
	if err := s.Store.SaveTenant(t); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	// Built-in default group: every machine of the tenant is a member.
	all := api.Group{ID: newID(), TenantID: t.ID, Name: "all", MatchAll: true, Created: t.Created}
	if err := s.Store.SaveGroup(all); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("tenant created", "tenant", t.ID, "name", t.Name, "default_group", all.ID)
	writeJSON(w, t)
}

func (s *AdminServer) listTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListTenants()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, ts)
}

func (s *AdminServer) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string            `json:"tenant_id"`
		Labels   map[string]string `json:"labels"`
		TTLSec   int64             `json:"ttl_sec"`  // 0 = default 24h; -1 = no expiry
		MaxUses  int               `json:"max_uses"` // 0/1 = single-use; -1 = unlimited; N = up to N enrollments
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TenantID == "" {
		http.Error(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	if _, err := s.Store.GetTenant(req.TenantID); err != nil {
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}
	now := time.Now().Unix()
	var expires int64
	switch {
	case req.TTLSec == 0:
		expires = now + 86400
	case req.TTLSec > 0:
		expires = now + req.TTLSec
	}
	t := api.EnrollToken{Token: newID() + newID(), TenantID: req.TenantID, Labels: req.Labels, Created: now, ExpiresAt: expires, MaxUses: req.MaxUses}
	if err := s.Store.SaveToken(t); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func (s *AdminServer) listMachines(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Store.ListMachines(r.URL.Query().Get("tenant"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, ms)
}

func (s *AdminServer) getMachine(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMachine(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, m)
}

// putDesired sets a machine's Override specs; effective desired state is
// recomputed (groups first, then override wins on name conflicts).
func (s *AdminServer) putDesired(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.Store.GetMachine(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Modules []api.ModuleSpec `json:"modules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.validateModules(w, req.Modules) {
		return
	}
	m.Override = req.Modules
	if err := s.Store.SaveMachine(m); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(m.TenantID, m.ID); err != nil {
		http.Error(w, "recompute error", http.StatusInternalServerError)
		return
	}
	m, _ = s.Store.GetMachine(id)
	writeJSON(w, m.Desired)
}

// uploadModule registers a payload and signs it with the module key.
func (s *AdminServer) uploadModule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Exec    string `json:"exec"`
		Payload []byte `json:"payload"` // base64 in JSON
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Version == "" || len(req.Payload) == 0 {
		http.Error(w, "name, version, payload required", http.StatusBadRequest)
		return
	}
	if req.Exec == "" {
		req.Exec = "payload"
	}
	a := api.ModuleArtifact{
		Name:      req.Name,
		Version:   req.Version,
		Exec:      req.Exec,
		Payload:   req.Payload,
		Signature: s.CA.SignModule(req.Payload),
	}
	if err := s.Store.SaveModule(a); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("module uploaded", "module", a.Name, "version", a.Version, "bytes", len(a.Payload))
	a.Payload, a.Signature = nil, nil
	writeJSON(w, a)
}

func (s *AdminServer) listModules(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Store.ListModules()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, ms)
}

func (s *AdminServer) healthz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.ListTenants(); err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// deleteMachine removes a machine and its memberships. This is also the
// revocation mechanism: identity checks resolve against the store on
// every request, so a deleted machine's certificate stops working
// immediately.
func (s *AdminServer) deleteMachine(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMachine(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.Store.DeleteMachine(m.ID); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	s.Bus.Publish(m.ID) // wake any live stream so it terminates promptly
	s.Log.Info("machine deleted", "machine", m.ID, "name", m.Name)
	w.WriteHeader(http.StatusNoContent)
}
