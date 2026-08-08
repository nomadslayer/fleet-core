// Package server hosts the two HTTP surfaces of the control plane:
//
//   - agent.go/server.go: the agent-facing mTLS API (this file)
//   - admin.go:           the operator-facing REST API
//
// Agent identity is taken exclusively from the mTLS client certificate
// (CN = machine ID, OU = tenant ID). Enrollment is the only endpoint
// reachable without a client cert and it burns a one-time token.
package server

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"fleetcore/internal/api"
	"fleetcore/internal/bus"
	"fleetcore/internal/ca"
	"fleetcore/internal/store"
)

type AgentServer struct {
	CA        *ca.CA
	Store     store.Store
	Bus       bus.Bus
	Log       *slog.Logger
	Resolver  *Resolver
	Live      *LiveRegistry
	Installer *Installer
}

// TLSConfig builds the mTLS listener config. Client certs are verified
// when presented; the identity middleware enforces presence per-route so
// that /v1/enroll can be reached by not-yet-enrolled agents.
func (s *AgentServer) TLSConfig(sans []string) (*tls.Config, error) {
	serverCert, err := s.CA.ServerTLSCert(sans)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.CA.Cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}, nil
}

func (s *AgentServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /download/{name}", s.handleDownload)
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.Handle("POST /v1/renew", s.withIdentity(s.handleRenew))
	mux.Handle("POST /v1/heartbeat", s.withIdentity(s.handleHeartbeat))
	mux.Handle("GET /v1/stream", s.withIdentity(s.handleStream))
	mux.Handle("GET /v1/module", s.withIdentity(s.handleModule))
	mux.Handle("POST /v1/status", s.withIdentity(s.handleStatus))
	mux.Handle("POST /v1/metrics", s.withIdentity(s.handleMetricsPush))
	return mux
}

type identity struct{ MachineID, TenantID string }

func (s *AgentServer) withIdentity(next func(http.ResponseWriter, *http.Request, identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		if len(leaf.Subject.OrganizationalUnit) == 0 {
			http.Error(w, "certificate missing tenant", http.StatusForbidden)
			return
		}
		next(w, r, identity{
			MachineID: leaf.Subject.CommonName,
			TenantID:  leaf.Subject.OrganizationalUnit[0],
		})
	})
}

func (s *AgentServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := s.Store.ConsumeToken(req.Token)
	if err != nil {
		s.Log.Warn("enroll rejected", "err", err)
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	machineID := newID()
	certPEM, err := s.CA.SignAgent([]byte(req.CSRPEM), machineID, tok.TenantID)
	if err != nil {
		s.Log.Error("sign agent", "err", err)
		http.Error(w, "csr rejected", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()
	m := api.Machine{ID: machineID, TenantID: tok.TenantID, Name: req.Name, Labels: tok.Labels, Enrolled: now, LastSeen: now}
	if err := s.Store.SaveMachine(m); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	modPub, err := s.CA.ModulePubPEM()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.Resolver.Recompute(tok.TenantID, machineID); err != nil {
		s.Log.Error("recompute after enroll", "err", err)
	}
	s.Log.Info("machine enrolled", "machine", machineID, "tenant", tok.TenantID, "name", req.Name)
	writeJSON(w, api.EnrollResponse{
		MachineID:    machineID,
		TenantID:     tok.TenantID,
		CertPEM:      string(certPEM),
		CAPEM:        string(s.CA.CertPEM),
		ModulePubPEM: string(modPub),
	})
}

func (s *AgentServer) handleHeartbeat(w http.ResponseWriter, r *http.Request, id identity) {
	var hb api.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&hb); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	err := s.Store.TouchHeartbeat(id.MachineID, time.Now().Unix(), hb.Inventory)
	if errors.Is(err, store.ErrNotFound) {
		// Machine was deleted: this is the revocation path — the cert is
		// cryptographically valid but the identity no longer exists.
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRenew re-issues the client certificate for a live, enrolled
// machine over its existing mTLS identity.
func (s *AgentServer) handleRenew(w http.ResponseWriter, r *http.Request, id identity) {
	if _, err := s.Store.GetMachine(id.MachineID); err != nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	var req struct {
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	certPEM, err := s.CA.SignAgent([]byte(req.CSRPEM), id.MachineID, id.TenantID)
	if err != nil {
		http.Error(w, "csr rejected", http.StatusBadRequest)
		return
	}
	s.Log.Info("certificate renewed", "machine", id.MachineID)
	writeJSON(w, map[string]string{"cert_pem": string(certPEM)})
}

// handleStream is the reconcile channel: an SSE stream that emits the
// machine's DesiredState on connect and again whenever it changes.
func (s *AgentServer) handleStream(w http.ResponseWriter, r *http.Request, id identity) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	sig, cancel := s.Bus.Subscribe(id.MachineID)
	defer cancel()

	send := func() bool {
		m, err := s.Store.GetMachine(id.MachineID)
		if err != nil {
			return false
		}
		raw, _ := json.Marshal(m.Desired)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sig:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *AgentServer) handleModule(w http.ResponseWriter, r *http.Request, id identity) {
	name, version := r.URL.Query().Get("name"), r.URL.Query().Get("version")
	a, err := s.Store.GetModule(name, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "module not found", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, a)
}

func (s *AgentServer) handleStatus(w http.ResponseWriter, r *http.Request, id identity) {
	var rep api.StatusReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rep); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	err := s.Store.SetStatus(id.MachineID, time.Now().Unix(), rep.Modules)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
