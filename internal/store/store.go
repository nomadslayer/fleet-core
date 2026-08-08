// Package store defines the persistence contract for the control plane.
// The core only ever talks to the Store interface; the bundled FileStore
// keeps the footprint at zero external dependencies. A Postgres
// implementation slots in behind the same interface later.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fleetcore/internal/api"
)

func timeNow() int64 { return time.Now().Unix() }

var (
	ErrNotFound  = errors.New("not found")
	ErrTokenUsed = errors.New("token unknown or already used")
)

type Store interface {
	SaveTenant(t api.Tenant) error
	GetTenant(id string) (api.Tenant, error)
	ListTenants() ([]api.Tenant, error)

	SaveToken(t api.EnrollToken) error
	// ConsumeToken atomically marks a token used and returns it.
	ConsumeToken(token string) (api.EnrollToken, error)

	SaveMachine(m api.Machine) error
	GetMachine(id string) (api.Machine, error)
	ListMachines(tenantID string) ([]api.Machine, error)
	DeleteMachine(id string) error
	// TouchHeartbeat and SetStatus update only their own columns so a
	// heartbeat racing a desired-state recompute cannot clobber it.
	TouchHeartbeat(id string, lastSeen int64, inv api.Inventory) error
	SetStatus(id string, lastSeen int64, status []api.ModuleStatus) error

	SaveModule(a api.ModuleArtifact) error
	GetModule(name, version string) (api.ModuleArtifact, error)
	ListModules() ([]api.ModuleArtifact, error)
	// DeleteModule drops an artifact. Ad-hoc commands are stored as modules
	// and would otherwise accumulate for the life of the control plane.
	DeleteModule(name, version string) error

	SaveGroup(g api.Group) error
	GetGroup(id string) (api.Group, error)
	ListGroups(tenantID string) ([]api.Group, error)
	DeleteGroup(id string) error
	AddGroupMember(groupID, machineID string) error
	RemoveGroupMember(groupID, machineID string) error
	ListGroupMembers(groupID string) ([]string, error)
}

// ---- FileStore ----

type fileData struct {
	Tenants  map[string]api.Tenant          `json:"tenants"`
	Tokens   map[string]api.EnrollToken     `json:"tokens"`
	Machines map[string]api.Machine         `json:"machines"`
	Modules  map[string]api.ModuleArtifact  `json:"modules"` // key: name@version
	Groups   map[string]api.Group           `json:"groups"`
	Members  map[string]map[string]struct{} `json:"members"` // groupID -> machineID set
}

type FileStore struct {
	mu   sync.Mutex
	path string
	data fileData
}

func OpenFile(path string) (*FileStore, error) {
	fs := &FileStore{path: path, data: fileData{
		Tenants:  map[string]api.Tenant{},
		Tokens:   map[string]api.EnrollToken{},
		Machines: map[string]api.Machine{},
		Modules:  map[string]api.ModuleArtifact{},
		Groups:   map[string]api.Group{},
		Members:  map[string]map[string]struct{}{},
	}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fresh store
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &fs.data); err != nil {
			return nil, fmt.Errorf("corrupt store %s: %w", path, err)
		}
		if fs.data.Groups == nil {
			fs.data.Groups = map[string]api.Group{}
		}
		if fs.data.Members == nil {
			fs.data.Members = map[string]map[string]struct{}{}
		}
	}
	return fs, nil
}

// flush writes atomically (temp file + rename). Callers hold fs.mu.
func (fs *FileStore) flush() error {
	raw, err := json.Marshal(fs.data)
	if err != nil {
		return err
	}
	tmp := fs.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(fs.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fs.path)
}

func (fs *FileStore) SaveTenant(t api.Tenant) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Tenants[t.ID] = t
	return fs.flush()
}

func (fs *FileStore) GetTenant(id string) (api.Tenant, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t, ok := fs.data.Tenants[id]
	if !ok {
		return api.Tenant{}, ErrNotFound
	}
	return t, nil
}

func (fs *FileStore) ListTenants() ([]api.Tenant, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]api.Tenant, 0, len(fs.data.Tenants))
	for _, t := range fs.data.Tenants {
		out = append(out, t)
	}
	return out, nil
}

func (fs *FileStore) SaveToken(t api.EnrollToken) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Tokens[t.Token] = t
	return fs.flush()
}

func (fs *FileStore) ConsumeToken(token string) (api.EnrollToken, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t, ok := fs.data.Tokens[token]
	if !ok || (t.ExpiresAt > 0 && t.ExpiresAt < timeNow()) {
		return api.EnrollToken{}, ErrTokenUsed
	}
	if t.Used && t.Uses == 0 {
		t.Uses = 1 // legacy single-use records
	}
	limit := t.MaxUses
	if limit == 0 {
		limit = 1
	}
	if limit > 0 && t.Uses >= limit {
		return api.EnrollToken{}, ErrTokenUsed
	}
	t.Uses++
	t.Used = limit > 0 && t.Uses >= limit
	fs.data.Tokens[token] = t
	if err := fs.flush(); err != nil {
		return api.EnrollToken{}, err
	}
	return t, nil
}

func (fs *FileStore) SaveMachine(m api.Machine) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Machines[m.ID] = m
	return fs.flush()
}

func (fs *FileStore) GetMachine(id string) (api.Machine, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	m, ok := fs.data.Machines[id]
	if !ok {
		return api.Machine{}, ErrNotFound
	}
	return m, nil
}

func (fs *FileStore) ListMachines(tenantID string) ([]api.Machine, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []api.Machine{}
	for _, m := range fs.data.Machines {
		if tenantID == "" || m.TenantID == tenantID {
			out = append(out, m)
		}
	}
	return out, nil
}

func moduleKey(name, version string) string { return name + "@" + version }

func (fs *FileStore) SaveModule(a api.ModuleArtifact) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Modules[moduleKey(a.Name, a.Version)] = a
	return fs.flush()
}

func (fs *FileStore) DeleteModule(name, version string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.data.Modules, moduleKey(name, version))
	return fs.flush()
}

func (fs *FileStore) GetModule(name, version string) (api.ModuleArtifact, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	a, ok := fs.data.Modules[moduleKey(name, version)]
	if !ok {
		return api.ModuleArtifact{}, ErrNotFound
	}
	return a, nil
}

func (fs *FileStore) ListModules() ([]api.ModuleArtifact, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]api.ModuleArtifact, 0, len(fs.data.Modules))
	for _, a := range fs.data.Modules {
		a.Payload = nil // listing does not ship payloads
		a.Signature = nil
		out = append(out, a)
	}
	return out, nil
}

func (fs *FileStore) SaveGroup(g api.Group) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Groups[g.ID] = g
	return fs.flush()
}

func (fs *FileStore) GetGroup(id string) (api.Group, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	g, ok := fs.data.Groups[id]
	if !ok {
		return api.Group{}, ErrNotFound
	}
	return g, nil
}

func (fs *FileStore) ListGroups(tenantID string) ([]api.Group, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []api.Group{}
	for _, g := range fs.data.Groups {
		if tenantID == "" || g.TenantID == tenantID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (fs *FileStore) DeleteGroup(id string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.data.Groups, id)
	delete(fs.data.Members, id)
	return fs.flush()
}

func (fs *FileStore) AddGroupMember(groupID, machineID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.data.Members[groupID] == nil {
		fs.data.Members[groupID] = map[string]struct{}{}
	}
	fs.data.Members[groupID][machineID] = struct{}{}
	return fs.flush()
}

func (fs *FileStore) RemoveGroupMember(groupID, machineID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.data.Members[groupID], machineID)
	return fs.flush()
}

func (fs *FileStore) ListGroupMembers(groupID string) ([]string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []string{}
	for id := range fs.data.Members[groupID] {
		out = append(out, id)
	}
	return out, nil
}

func (fs *FileStore) DeleteMachine(id string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.data.Machines, id)
	for gid := range fs.data.Members {
		delete(fs.data.Members[gid], id)
	}
	return fs.flush()
}

func (fs *FileStore) TouchHeartbeat(id string, lastSeen int64, inv api.Inventory) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	m, ok := fs.data.Machines[id]
	if !ok {
		return ErrNotFound
	}
	m.LastSeen = lastSeen
	m.Inventory = inv
	fs.data.Machines[id] = m
	return fs.flush()
}

func (fs *FileStore) SetStatus(id string, lastSeen int64, status []api.ModuleStatus) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	m, ok := fs.data.Machines[id]
	if !ok {
		return ErrNotFound
	}
	m.LastSeen = lastSeen
	m.Status = status
	fs.data.Machines[id] = m
	return fs.flush()
}
