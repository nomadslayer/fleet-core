package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fleetcore/internal/api"
)

// TursoStore implements Store against Turso / libSQL over the Hrana
// HTTP API (`POST /v2/pipeline`). Implemented directly on net/http to
// stay dependency-free; any sqld-compatible endpoint works, so the same
// code covers Turso cloud and a self-hosted sqld.
//
// Record fields with nested structure (inventory, desired, status) are
// stored as JSON text columns: the control plane is the only writer and
// reads are always whole-record, so relational decomposition would buy
// nothing at this stage.
type TursoStore struct {
	url    string // e.g. https://mydb-org.turso.io
	token  string
	client *http.Client
}

func OpenTurso(url, token string) (*TursoStore, error) {
	ts := &TursoStore{
		url:    url,
		token:  token,
		client: &http.Client{Timeout: 15 * time.Second},
	}
	if err := ts.migrate(); err != nil {
		return nil, fmt.Errorf("turso migrate: %w", err)
	}
	return ts, nil
}

func (ts *TursoStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, created INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			token TEXT PRIMARY KEY, tenant_id TEXT NOT NULL,
			created INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS machines (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			enrolled INTEGER NOT NULL, last_seen INTEGER NOT NULL,
			inventory TEXT NOT NULL DEFAULT '{}',
			desired   TEXT NOT NULL DEFAULT '{"revision":0,"modules":null}',
			status    TEXT NOT NULL DEFAULT 'null')`,
		`CREATE INDEX IF NOT EXISTS machines_tenant ON machines(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL,
			selector TEXT NOT NULL DEFAULT '{}', modules TEXT NOT NULL DEFAULT '[]',
			created INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS groups_tenant ON groups(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS group_members (
			group_id TEXT NOT NULL, machine_id TEXT NOT NULL,
			PRIMARY KEY (group_id, machine_id))`,
		`CREATE TABLE IF NOT EXISTS modules (
			name TEXT NOT NULL, version TEXT NOT NULL, exec TEXT NOT NULL,
			payload BLOB NOT NULL, signature BLOB NOT NULL,
			PRIMARY KEY (name, version))`,
	}
	for _, s := range stmts {
		if _, err := ts.exec(s); err != nil {
			return err
		}
	}
	// Column upgrades for stores created before groups existed; duplicate
	// column errors are the expected no-op on fresh/upgraded schemas.
	for _, alter := range []string{
		`ALTER TABLE machines ADD COLUMN labels TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE machines ADD COLUMN override TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE tokens ADD COLUMN labels TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE groups ADD COLUMN match_all INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tokens ADD COLUMN max_uses INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE tokens ADD COLUMN uses INTEGER NOT NULL DEFAULT 0`,
		`UPDATE tokens SET uses=1 WHERE used=1 AND uses=0`,
	} {
		if _, err := ts.exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// ---- Hrana wire types ----

type hranaValue struct {
	Type   string `json:"type"`             // text | integer | blob | null | float
	Value  string `json:"value,omitempty"`  // text/integer/float payload
	Base64 string `json:"base64,omitempty"` // blob payload
}

type hranaStmt struct {
	SQL  string       `json:"sql"`
	Args []hranaValue `json:"args,omitempty"`
}

type hranaRequest struct {
	Type string     `json:"type"` // execute | close
	Stmt *hranaStmt `json:"stmt,omitempty"`
}

type hranaResult struct {
	Cols []struct {
		Name string `json:"name"`
	} `json:"cols"`
	Rows             [][]hranaValue `json:"rows"`
	AffectedRowCount int64          `json:"affected_row_count"`
}

type hranaPipelineResponse struct {
	Results []struct {
		Type     string `json:"type"` // ok | error
		Response *struct {
			Type   string       `json:"type"`
			Result *hranaResult `json:"result"`
		} `json:"response"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"results"`
}

func arg(v any) hranaValue {
	switch x := v.(type) {
	case string:
		return hranaValue{Type: "text", Value: x}
	case int:
		return hranaValue{Type: "integer", Value: strconv.Itoa(x)}
	case int64:
		return hranaValue{Type: "integer", Value: strconv.FormatInt(x, 10)}
	case bool:
		if x {
			return hranaValue{Type: "integer", Value: "1"}
		}
		return hranaValue{Type: "integer", Value: "0"}
	case []byte:
		return hranaValue{Type: "blob", Base64: base64.StdEncoding.EncodeToString(x)}
	case nil:
		return hranaValue{Type: "null"}
	default:
		panic(fmt.Sprintf("unsupported arg type %T", v))
	}
}

// exec runs one statement and returns its result.
func (ts *TursoStore) exec(sql string, args ...any) (*hranaResult, error) {
	hargs := make([]hranaValue, len(args))
	for i, a := range args {
		hargs[i] = arg(a)
	}
	body, err := json.Marshal(map[string]any{
		"requests": []hranaRequest{
			{Type: "execute", Stmt: &hranaStmt{SQL: sql, Args: hargs}},
			{Type: "close"},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ts.url+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ts.token != "" {
		req.Header.Set("Authorization", "Bearer "+ts.token)
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("turso: %s", resp.Status)
	}
	var pr hranaPipelineResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if len(pr.Results) == 0 {
		return nil, errors.New("turso: empty pipeline response")
	}
	r := pr.Results[0]
	if r.Type != "ok" {
		if r.Error != nil {
			return nil, fmt.Errorf("turso: %s", r.Error.Message)
		}
		return nil, errors.New("turso: statement failed")
	}
	if r.Response == nil || r.Response.Result == nil {
		return nil, errors.New("turso: malformed response")
	}
	return r.Response.Result, nil
}

// ---- row decoding helpers ----

func (v hranaValue) str() string { return v.Value }

func (v hranaValue) i64() int64 {
	n, _ := strconv.ParseInt(v.Value, 10, 64)
	return n
}

// blob decodes a Hrana blob value. Hrana encodes blobs as base64 WITHOUT
// padding, which StdEncoding rejects outright — so decoding a module with
// StdEncoding truncates both the payload and its signature at the last
// whole 4-character group, and the agent then rejects the module as
// "payload signature invalid". Tolerate padded input too, since the spec
// does not forbid it and some servers pad.
func (v hranaValue) blob() []byte {
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(v.Base64, "="))
	if err != nil {
		return nil
	}
	return b
}

// ---- Store implementation ----

func (ts *TursoStore) SaveTenant(t api.Tenant) error {
	_, err := ts.exec(
		`INSERT INTO tenants (id, name, created) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		t.ID, t.Name, t.Created)
	return err
}

func (ts *TursoStore) GetTenant(id string) (api.Tenant, error) {
	r, err := ts.exec(`SELECT id, name, created FROM tenants WHERE id=?`, id)
	if err != nil {
		return api.Tenant{}, err
	}
	if len(r.Rows) == 0 {
		return api.Tenant{}, ErrNotFound
	}
	row := r.Rows[0]
	return api.Tenant{ID: row[0].str(), Name: row[1].str(), Created: row[2].i64()}, nil
}

func (ts *TursoStore) ListTenants() ([]api.Tenant, error) {
	r, err := ts.exec(`SELECT id, name, created FROM tenants ORDER BY created`)
	if err != nil {
		return nil, err
	}
	out := make([]api.Tenant, 0, len(r.Rows))
	for _, row := range r.Rows {
		out = append(out, api.Tenant{ID: row[0].str(), Name: row[1].str(), Created: row[2].i64()})
	}
	return out, nil
}

func (ts *TursoStore) SaveToken(t api.EnrollToken) error {
	lbl, err := json.Marshal(orEmptyMap(t.Labels))
	if err != nil {
		return err
	}
	mu := t.MaxUses
	if mu == 0 {
		mu = 1
	}
	_, err = ts.exec(
		`INSERT INTO tokens (token, tenant_id, labels, created, expires_at, max_uses, uses, used) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Token, t.TenantID, string(lbl), t.Created, t.ExpiresAt, mu, t.Uses, t.Used)
	return err
}

// ConsumeToken is a single atomic UPDATE ... RETURNING: concurrent
// enrollments with the same token race on used=0 and exactly one wins.
func (ts *TursoStore) ConsumeToken(token string) (api.EnrollToken, error) {
	r, err := ts.exec(
		`UPDATE tokens SET uses=uses+1, used=(max_uses>0 AND uses+1>=max_uses)
		 WHERE token=? AND (expires_at=0 OR expires_at>?)
		   AND (max_uses<0 OR uses<max_uses)
		 RETURNING token, tenant_id, labels, created`, token, time.Now().Unix())
	if err != nil {
		return api.EnrollToken{}, err
	}
	if len(r.Rows) == 0 {
		return api.EnrollToken{}, ErrTokenUsed
	}
	row := r.Rows[0]
	out := api.EnrollToken{Token: row[0].str(), TenantID: row[1].str(), Created: row[3].i64(), Used: true}
	_ = json.Unmarshal([]byte(row[2].str()), &out.Labels)
	return out, nil
}

func (ts *TursoStore) SaveMachine(m api.Machine) error {
	inv, err := json.Marshal(m.Inventory)
	if err != nil {
		return err
	}
	des, err := json.Marshal(m.Desired)
	if err != nil {
		return err
	}
	st, err := json.Marshal(m.Status)
	if err != nil {
		return err
	}
	lbl, err := json.Marshal(orEmptyMap(m.Labels))
	if err != nil {
		return err
	}
	ovr, err := json.Marshal(orEmptyList(m.Override))
	if err != nil {
		return err
	}
	_, err = ts.exec(
		`INSERT INTO machines (id, tenant_id, name, labels, override, enrolled, last_seen, inventory, desired, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name, labels=excluded.labels, override=excluded.override,
		   last_seen=excluded.last_seen,
		   inventory=excluded.inventory, desired=excluded.desired, status=excluded.status`,
		m.ID, m.TenantID, m.Name, string(lbl), string(ovr), m.Enrolled, m.LastSeen, string(inv), string(des), string(st))
	return err
}

func scanMachine(row []hranaValue) (api.Machine, error) {
	m := api.Machine{
		ID:       row[0].str(),
		TenantID: row[1].str(),
		Name:     row[2].str(),
		Enrolled: row[5].i64(),
		LastSeen: row[6].i64(),
	}
	if err := json.Unmarshal([]byte(row[3].str()), &m.Labels); err != nil {
		return m, fmt.Errorf("machine %s labels: %w", m.ID, err)
	}
	if err := json.Unmarshal([]byte(row[4].str()), &m.Override); err != nil {
		return m, fmt.Errorf("machine %s override: %w", m.ID, err)
	}
	if err := json.Unmarshal([]byte(row[7].str()), &m.Inventory); err != nil {
		return m, fmt.Errorf("machine %s inventory: %w", m.ID, err)
	}
	if err := json.Unmarshal([]byte(row[8].str()), &m.Desired); err != nil {
		return m, fmt.Errorf("machine %s desired: %w", m.ID, err)
	}
	if err := json.Unmarshal([]byte(row[9].str()), &m.Status); err != nil {
		return m, fmt.Errorf("machine %s status: %w", m.ID, err)
	}
	return m, nil
}

const machineCols = `id, tenant_id, name, labels, override, enrolled, last_seen, inventory, desired, status`

func (ts *TursoStore) GetMachine(id string) (api.Machine, error) {
	r, err := ts.exec(`SELECT `+machineCols+` FROM machines WHERE id=?`, id)
	if err != nil {
		return api.Machine{}, err
	}
	if len(r.Rows) == 0 {
		return api.Machine{}, ErrNotFound
	}
	return scanMachine(r.Rows[0])
}

func (ts *TursoStore) ListMachines(tenantID string) ([]api.Machine, error) {
	var (
		r   *hranaResult
		err error
	)
	if tenantID == "" {
		r, err = ts.exec(`SELECT ` + machineCols + ` FROM machines ORDER BY enrolled`)
	} else {
		r, err = ts.exec(`SELECT `+machineCols+` FROM machines WHERE tenant_id=? ORDER BY enrolled`, tenantID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.Machine, 0, len(r.Rows))
	for _, row := range r.Rows {
		m, err := scanMachine(row)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (ts *TursoStore) SaveModule(a api.ModuleArtifact) error {
	_, err := ts.exec(
		`INSERT INTO modules (name, version, exec, payload, signature) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name, version) DO UPDATE SET
		   exec=excluded.exec, payload=excluded.payload, signature=excluded.signature`,
		a.Name, a.Version, a.Exec, a.Payload, a.Signature)
	return err
}

func (ts *TursoStore) GetModule(name, version string) (api.ModuleArtifact, error) {
	r, err := ts.exec(
		`SELECT name, version, exec, payload, signature FROM modules WHERE name=? AND version=?`,
		name, version)
	if err != nil {
		return api.ModuleArtifact{}, err
	}
	if len(r.Rows) == 0 {
		return api.ModuleArtifact{}, ErrNotFound
	}
	row := r.Rows[0]
	return api.ModuleArtifact{
		Name:      row[0].str(),
		Version:   row[1].str(),
		Exec:      row[2].str(),
		Payload:   row[3].blob(),
		Signature: row[4].blob(),
	}, nil
}

func (ts *TursoStore) DeleteModule(name, version string) error {
	_, err := ts.exec(`DELETE FROM modules WHERE name=? AND version=?`, name, version)
	return err
}

func (ts *TursoStore) ListModules() ([]api.ModuleArtifact, error) {
	r, err := ts.exec(`SELECT name, version, exec FROM modules ORDER BY name, version`)
	if err != nil {
		return nil, err
	}
	out := make([]api.ModuleArtifact, 0, len(r.Rows))
	for _, row := range r.Rows {
		out = append(out, api.ModuleArtifact{Name: row[0].str(), Version: row[1].str(), Exec: row[2].str()})
	}
	return out, nil
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func orEmptyList(l []api.ModuleSpec) []api.ModuleSpec {
	if l == nil {
		return []api.ModuleSpec{}
	}
	return l
}

func (ts *TursoStore) SaveGroup(g api.Group) error {
	sel, err := json.Marshal(orEmptyMap(g.Selector))
	if err != nil {
		return err
	}
	mods, err := json.Marshal(orEmptyList(g.Modules))
	if err != nil {
		return err
	}
	_, err = ts.exec(
		`INSERT INTO groups (id, tenant_id, name, match_all, selector, modules, created) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name, match_all=excluded.match_all, selector=excluded.selector, modules=excluded.modules`,
		g.ID, g.TenantID, g.Name, g.MatchAll, string(sel), string(mods), g.Created)
	return err
}

func scanGroup(row []hranaValue) (api.Group, error) {
	g := api.Group{ID: row[0].str(), TenantID: row[1].str(), Name: row[2].str(), MatchAll: row[3].i64() == 1, Created: row[6].i64()}
	if err := json.Unmarshal([]byte(row[4].str()), &g.Selector); err != nil {
		return g, fmt.Errorf("group %s selector: %w", g.ID, err)
	}
	if err := json.Unmarshal([]byte(row[5].str()), &g.Modules); err != nil {
		return g, fmt.Errorf("group %s modules: %w", g.ID, err)
	}
	return g, nil
}

func (ts *TursoStore) GetGroup(id string) (api.Group, error) {
	r, err := ts.exec(`SELECT id, tenant_id, name, match_all, selector, modules, created FROM groups WHERE id=?`, id)
	if err != nil {
		return api.Group{}, err
	}
	if len(r.Rows) == 0 {
		return api.Group{}, ErrNotFound
	}
	return scanGroup(r.Rows[0])
}

func (ts *TursoStore) ListGroups(tenantID string) ([]api.Group, error) {
	var (
		r   *hranaResult
		err error
	)
	if tenantID == "" {
		r, err = ts.exec(`SELECT id, tenant_id, name, match_all, selector, modules, created FROM groups ORDER BY name`)
	} else {
		r, err = ts.exec(`SELECT id, tenant_id, name, match_all, selector, modules, created FROM groups WHERE tenant_id=? ORDER BY name`, tenantID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.Group, 0, len(r.Rows))
	for _, row := range r.Rows {
		g, err := scanGroup(row)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (ts *TursoStore) DeleteGroup(id string) error {
	if _, err := ts.exec(`DELETE FROM group_members WHERE group_id=?`, id); err != nil {
		return err
	}
	_, err := ts.exec(`DELETE FROM groups WHERE id=?`, id)
	return err
}

func (ts *TursoStore) AddGroupMember(groupID, machineID string) error {
	_, err := ts.exec(
		`INSERT INTO group_members (group_id, machine_id) VALUES (?, ?)
		 ON CONFLICT(group_id, machine_id) DO NOTHING`, groupID, machineID)
	return err
}

func (ts *TursoStore) RemoveGroupMember(groupID, machineID string) error {
	_, err := ts.exec(`DELETE FROM group_members WHERE group_id=? AND machine_id=?`, groupID, machineID)
	return err
}

func (ts *TursoStore) ListGroupMembers(groupID string) ([]string, error) {
	r, err := ts.exec(`SELECT machine_id FROM group_members WHERE group_id=?`, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		out = append(out, row[0].str())
	}
	return out, nil
}

func (ts *TursoStore) DeleteMachine(id string) error {
	if _, err := ts.exec(`DELETE FROM group_members WHERE machine_id=?`, id); err != nil {
		return err
	}
	_, err := ts.exec(`DELETE FROM machines WHERE id=?`, id)
	return err
}

func (ts *TursoStore) TouchHeartbeat(id string, lastSeen int64, inv api.Inventory) error {
	raw, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	r, err := ts.exec(`UPDATE machines SET last_seen=?, inventory=? WHERE id=?`, lastSeen, string(raw), id)
	if err != nil {
		return err
	}
	if r.AffectedRowCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (ts *TursoStore) SetStatus(id string, lastSeen int64, status []api.ModuleStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	r, err := ts.exec(`UPDATE machines SET last_seen=?, status=? WHERE id=?`, lastSeen, string(raw), id)
	if err != nil {
		return err
	}
	if r.AffectedRowCount == 0 {
		return ErrNotFound
	}
	return nil
}
