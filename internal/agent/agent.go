// Package agent implements the on-host runtime. Design constraints:
//
//   - outbound-only: the agent opens all connections (works behind NAT),
//   - dumb transport: no feature logic lives here; it reconciles the
//     desired module set by fetching signed payloads and executing them,
//   - minimal footprint: stdlib only, single static binary, state under
//     one directory.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fleetcore/internal/api"
)

const Version = "0.1.0"

type Agent struct {
	ServerURL string // e.g. https://cp.example.com:8443
	DataDir   string // e.g. /var/lib/fleet-agent
	Log       *slog.Logger

	Heartbeat    time.Duration
	MetricsEvery time.Duration // 0 disables live metrics push
	TopProcesses int           // top-N processes in each sample (default 5)

	client    *http.Client
	machineID string
	modKey    []byte // PEM of ed25519 module verification key

	mu          sync.Mutex
	lastDesired *api.DesiredState // for periodic re-reconcile (failed-module retry)
	updates     updateCache       // cached pending-update snapshot
}

// ---- enrollment ----

// Enroll performs first contact: generate a key, send CSR + one-time
// token, persist the issued identity and pinned CA.
//
// caFingerprint (hex sha256 of the CA cert DER) authenticates the server
// on first contact; if empty, trust-on-first-use is applied and the CA
// is pinned for every subsequent connection.
func (a *Agent) Enroll(token, name, caFingerprint string) error {
	if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// Bootstrap TLS: no CA pinned yet, so verify by fingerprint (or TOFU).
	bootstrap := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // custom verification below
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if caFingerprint == "" {
					return nil // TOFU; CA is pinned from the enroll response
				}
				for _, raw := range rawCerts {
					sum := sha256.Sum256(raw)
					if strings.EqualFold(hex.EncodeToString(sum[:]), caFingerprint) {
						return nil
					}
				}
				return errors.New("server fingerprint mismatch")
			},
		},
	}}
	if caFingerprint == "" {
		a.Log.Warn("no --fingerprint given: trusting server on first use")
	}

	body, _ := json.Marshal(api.EnrollRequest{Token: token, CSRPEM: string(csrPEM), Name: name})
	resp, err := bootstrap.Post(a.ServerURL+"/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("enroll request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("enroll rejected: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var er api.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"key.pem":        pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		"cert.pem":       []byte(er.CertPEM),
		"ca.pem":         []byte(er.CAPEM),
		"module-pub.pem": []byte(er.ModulePubPEM),
		"machine-id":     []byte(er.MachineID),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(a.DataDir, name), data, 0o600); err != nil {
			return err
		}
	}
	a.Log.Info("enrolled", "machine", er.MachineID, "tenant", er.TenantID)
	return nil
}

// Enrolled reports whether identity material exists.
func (a *Agent) Enrolled() bool {
	_, err := os.Stat(filepath.Join(a.DataDir, "cert.pem"))
	return err == nil
}

// ---- runtime ----

func (a *Agent) init() error {
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(a.DataDir, "cert.pem"),
		filepath.Join(a.DataDir, "key.pem"),
	)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(a.DataDir, "ca.pem"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("invalid pinned CA")
	}
	id, err := os.ReadFile(filepath.Join(a.DataDir, "machine-id"))
	if err != nil {
		return err
	}
	a.machineID = strings.TrimSpace(string(id))
	a.modKey, err = os.ReadFile(filepath.Join(a.DataDir, "module-pub.pem"))
	if err != nil {
		return err
	}
	a.client = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
		},
	}}
	return nil
}

// Run starts the heartbeat and reconcile loops and blocks until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.init(); err != nil {
		return err
	}
	if a.Heartbeat <= 0 {
		a.Heartbeat = 30 * time.Second
	}
	a.maybeRenewCert(ctx)
	go a.heartbeatLoop(ctx)
	go a.metricsLoop(ctx)
	go a.retryLoop(ctx)
	a.streamLoop(ctx)
	return ctx.Err()
}

// maybeRenewCert re-issues the client certificate when it is within 30
// days of expiry, reusing the existing key. Best-effort: failure only
// logs — the current cert still works until NotAfter.
func (a *Agent) maybeRenewCert(ctx context.Context) {
	certPEM, err := os.ReadFile(filepath.Join(a.DataDir, "cert.pem"))
	if err != nil {
		return
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	if time.Until(leaf.NotAfter) > 30*24*time.Hour {
		return
	}
	a.Log.Info("client certificate near expiry, renewing", "not_after", leaf.NotAfter)
	keyDER, err := os.ReadFile(filepath.Join(a.DataDir, "key.pem"))
	if err != nil {
		return
	}
	kb, _ := pem.Decode(keyDER)
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		a.Log.Error("renew: parse key", "err", err)
		return
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		a.Log.Error("renew: csr", "err", err)
		return
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	var resp struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := a.postJSON(ctx, "/v1/renew", map[string]string{"csr_pem": string(csrPEM)}, &resp); err != nil {
		a.Log.Error("renew failed", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(a.DataDir, "cert.pem"), []byte(resp.CertPEM), 0o600); err != nil {
		a.Log.Error("renew: write cert", "err", err)
		return
	}
	// Reload the client so new connections use the renewed cert.
	if err := a.init(); err != nil {
		a.Log.Error("renew: reload identity", "err", err)
		return
	}
	a.Log.Info("client certificate renewed")
}

// retryLoop re-runs reconcile against the last known desired state every
// 5 minutes, so modules that failed transiently converge without waiting
// for the next desired-state change.
func (a *Agent) retryLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			ds := a.lastDesired
			a.mu.Unlock()
			if ds != nil {
				a.reconcile(ctx, *ds)
			}
		}
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.Heartbeat)
	defer t.Stop()
	send := func() {
		inv := CollectInventory()
		// Attach the cached update snapshot, refreshed at most every 6h.
		inv.Updates = a.updatesSnapshot(ctx, 6*time.Hour)
		hb := api.Heartbeat{Inventory: inv}
		if err := a.postJSON(ctx, "/v1/heartbeat", hb, nil); err != nil {
			a.Log.Warn("heartbeat failed", "err", err)
		}
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

// streamLoop keeps one SSE connection open and reconciles on every
// desired-state event. Reconnects with capped backoff.
func (a *Agent) streamLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := a.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		a.Log.Warn("stream disconnected", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) streamOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.ServerURL+"/v1/stream", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: %s", resp.Status)
	}
	a.Log.Info("stream connected")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // keepalives, blank lines
		}
		var ds api.DesiredState
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ds); err != nil {
			a.Log.Warn("bad desired-state event", "err", err)
			continue
		}
		a.mu.Lock()
		a.lastDesired = &ds
		a.mu.Unlock()
		a.reconcile(ctx, ds)
	}
	return sc.Err()
}

// ---- http helpers ----

func (a *Agent) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (a *Agent) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.ServerURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
