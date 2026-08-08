// fleet-server: the control plane. Two listeners:
//   - agent API  (mTLS, internet-facing, outbound-only agents dial in)
//   - admin API  (bearer token, bind to localhost / private network)
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fleetcore/internal/bus"
	"fleetcore/internal/ca"
	"fleetcore/internal/server"
	"fleetcore/internal/store"
)

func main() {
	var (
		dataDir    = flag.String("data", "/var/lib/fleet-server", "data directory (CA, store)")
		agentAddr  = flag.String("agent-addr", ":8443", "agent API listen address (mTLS)")
		adminAddr  = flag.String("admin-addr", "127.0.0.1:8080", "admin API listen address")
		adminToken = flag.String("admin-token", os.Getenv("FLEET_ADMIN_TOKEN"), "admin bearer token (or FLEET_ADMIN_TOKEN)")
		sans       = flag.String("san", "localhost,127.0.0.1", "comma-separated SANs for the server certificate")
		dbURL      = flag.String("db-url", os.Getenv("FLEET_DB_URL"), "Turso/libSQL HTTP URL, e.g. https://mydb-org.turso.io (or FLEET_DB_URL); empty = local JSON file store")
		dbToken    = flag.String("db-token", os.Getenv("FLEET_DB_TOKEN"), "Turso auth token (or FLEET_DB_TOKEN)")
		binaries   = flag.String("binaries", "", "directory with fleet-agent-linux-{amd64,arm64}; enables /install.sh and /download")
		cmdHistory = flag.Int("max-command-history", 20, "ad-hoc commands retained per group/machine; oldest are dropped and their payloads deleted")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *adminToken == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			log.Error("rand", "err", err)
			os.Exit(1)
		}
		*adminToken = hex.EncodeToString(b)
		log.Warn("no admin token supplied; generated one for this run", "token", *adminToken)
	}

	authority, err := ca.LoadOrCreate(filepath.Join(*dataDir, "ca"))
	if err != nil {
		log.Error("ca init", "err", err)
		os.Exit(1)
	}
	fp := sha256.Sum256(authority.Cert.Raw)
	fpHex := hex.EncodeToString(fp[:])
	log.Info("certificate authority ready", "ca_sha256", fpHex)

	var st store.Store
	if *dbURL != "" {
		ts, err := store.OpenTurso(*dbURL, *dbToken)
		if err != nil {
			log.Error("turso store init", "err", err)
			os.Exit(1)
		}
		st = ts
		log.Info("store: turso/libsql", "url", *dbURL)
	} else {
		fs, err := store.OpenFile(filepath.Join(*dataDir, "store.json"))
		if err != nil {
			log.Error("file store init", "err", err)
			os.Exit(1)
		}
		st = fs
		log.Info("store: local json file")
	}
	b := bus.NewInProc()

	resolver := &server.Resolver{Store: st, Bus: b, Log: log}
	live := server.NewLiveRegistry()
	agentSrv := &server.AgentServer{CA: authority, Store: st, Bus: b, Log: log, Resolver: resolver, Live: live,
		Installer: &server.Installer{BinDir: *binaries, CAFingerprint: fpHex}}
	tlsCfg, err := agentSrv.TLSConfig(splitCSV(*sans))
	if err != nil {
		log.Error("tls config", "err", err)
		os.Exit(1)
	}
	agentHTTP := &http.Server{
		Addr:        *agentAddr,
		Handler:     agentSrv.Handler(),
		TLSConfig:   tlsCfg,
		ReadTimeout: 30 * time.Second,
		// no WriteTimeout: /v1/stream is long-lived
	}

	adminSrv := &server.AdminServer{CA: authority, Store: st, Bus: b, Token: *adminToken, Log: log, Resolver: resolver, Live: live,
		MaxCommandHistory: *cmdHistory}
	adminHTTP := &http.Server{
		Addr:         *adminAddr,
		Handler:      adminSrv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		log.Info("agent API listening", "addr", *agentAddr)
		ln, err := net.Listen("tcp", *agentAddr)
		if err != nil {
			log.Error("agent listen", "err", err)
			os.Exit(1)
		}
		if err := agentHTTP.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Error("agent serve", "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		log.Info("admin API listening", "addr", *adminAddr)
		if err := adminHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("admin serve", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Shutdown drains normal requests; long-lived SSE streams never end on
	// their own, so force-close whatever remains after the drain window.
	_ = agentHTTP.Shutdown(shutdownCtx)
	_ = agentHTTP.Close()
	_ = adminHTTP.Shutdown(shutdownCtx)
	_ = adminHTTP.Close()
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
