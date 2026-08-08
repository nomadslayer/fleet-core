package store

import (
	"bytes"
	"net/http"
	"os"
	"testing"

	"fleetcore/internal/api"
)

// TestTursoStore runs against a Hrana endpoint given via HRANA_URL
// (scripts/hrana_emulator.py locally, or a real Turso DB in CI).
func TestTursoStore(t *testing.T) {
	url := os.Getenv("HRANA_URL")
	if url == "" {
		t.Skip("HRANA_URL not set")
	}
	ts, err := OpenTurso(url, os.Getenv("HRANA_TOKEN"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// tenants
	if err := ts.SaveTenant(api.Tenant{ID: "t1", Name: "acme", Created: 100}); err != nil {
		t.Fatalf("save tenant: %v", err)
	}
	ten, err := ts.GetTenant("t1")
	if err != nil || ten.Name != "acme" {
		t.Fatalf("get tenant: %+v err=%v", ten, err)
	}
	if _, err := ts.GetTenant("nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// tokens: single-use semantics
	if err := ts.SaveToken(api.EnrollToken{Token: "tok1", TenantID: "t1", Created: 101}); err != nil {
		t.Fatalf("save token: %v", err)
	}
	tok, err := ts.ConsumeToken("tok1")
	if err != nil || tok.TenantID != "t1" || !tok.Used {
		t.Fatalf("consume: %+v err=%v", tok, err)
	}
	if _, err := ts.ConsumeToken("tok1"); err != ErrTokenUsed {
		t.Fatalf("reuse: expected ErrTokenUsed, got %v", err)
	}

	// machines: JSON round-trip incl. nested config
	m := api.Machine{
		ID: "m1", TenantID: "t1", Name: "web-01", Enrolled: 102, LastSeen: 103,
		Inventory: api.Inventory{Hostname: "web-01", OS: "ubuntu", OSVersion: "24.04", Packages: 870},
		Desired: api.DesiredState{Revision: 2, Modules: []api.ModuleSpec{
			{Name: "hello", Version: "1.0.0", Config: map[string]string{"greeting": "kev"}},
		}},
		Status: []api.ModuleStatus{{Name: "hello", Version: "1.0.0", State: "applied", AtUnix: 104}},
	}
	if err := ts.SaveMachine(m); err != nil {
		t.Fatalf("save machine: %v", err)
	}
	m.LastSeen = 200 // upsert path
	if err := ts.SaveMachine(m); err != nil {
		t.Fatalf("upsert machine: %v", err)
	}
	got, err := ts.GetMachine("m1")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.LastSeen != 200 || got.Desired.Revision != 2 ||
		got.Desired.Modules[0].Config["greeting"] != "kev" ||
		got.Status[0].State != "applied" || got.Inventory.Packages != 870 {
		t.Fatalf("machine round-trip mismatch: %+v", got)
	}
	list, err := ts.ListMachines("t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list machines: n=%d err=%v", len(list), err)
	}
	if list, _ := ts.ListMachines("other"); len(list) != 0 {
		t.Fatalf("tenant filter leaked: %+v", list)
	}

	// modules: blob round-trip byte-exact
	payload := bytes.Repeat([]byte{0x00, 0xff, 0x10, 0x9c}, 256)
	if err := ts.SaveModule(api.ModuleArtifact{
		Name: "hello", Version: "1.0.0", Exec: "payload",
		Payload: payload, Signature: []byte("sig-bytes"),
	}); err != nil {
		t.Fatalf("save module: %v", err)
	}
	art, err := ts.GetModule("hello", "1.0.0")
	if err != nil {
		t.Fatalf("get module: %v", err)
	}
	if !bytes.Equal(art.Payload, payload) || string(art.Signature) != "sig-bytes" {
		t.Fatalf("blob round-trip mismatch")
	}
	mods, err := ts.ListModules()
	if err != nil || len(mods) != 1 || mods[0].Payload != nil {
		t.Fatalf("list modules must omit payloads: %+v err=%v", mods, err)
	}
}

// Guard: Store interface satisfied by both implementations.
var (
	_ Store = (*FileStore)(nil)
	_ Store = (*TursoStore)(nil)
)

// Silence unused import when skipping.
var _ = http.MethodPost
