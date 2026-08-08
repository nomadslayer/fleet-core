// fleet-agent: single static binary, run as a systemd unit.
//
//	fleet-agent enroll --server https://cp:8443 --token <t> [--fingerprint <sha256>] [--name web-01]
//	fleet-agent run    --server https://cp:8443
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fleetcore/internal/agent"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	switch os.Args[1] {
	case "enroll":
		fs := flag.NewFlagSet("enroll", flag.ExitOnError)
		server := fs.String("server", "", "control plane URL, e.g. https://cp.example.com:8443")
		data := fs.String("data", "/var/lib/fleet-agent", "agent data directory")
		token := fs.String("token", "", "one-time enrollment token")
		fp := fs.String("fingerprint", "", "sha256 fingerprint of the control-plane CA (recommended)")
		name := fs.String("name", hostnameOr(""), "machine display name")
		_ = fs.Parse(os.Args[2:])
		if *server == "" || *token == "" {
			fmt.Fprintln(os.Stderr, "enroll requires --server and --token")
			os.Exit(2)
		}
		a := &agent.Agent{ServerURL: *server, DataDir: *data, Log: log}
		if a.Enrolled() {
			log.Error("already enrolled; remove the data directory to re-enroll", "dir", *data)
			os.Exit(1)
		}
		if err := a.Enroll(*token, *name, *fp); err != nil {
			log.Error("enroll failed", "err", err)
			os.Exit(1)
		}

	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		server := fs.String("server", "", "control plane URL")
		data := fs.String("data", "/var/lib/fleet-agent", "agent data directory")
		hb := fs.Duration("heartbeat", 30*time.Second, "heartbeat interval")
		me := fs.Duration("metrics-interval", 10*time.Second, "live metrics push interval (0 disables)")
		tp := fs.Int("top-processes", 5, "number of top processes by CPU to include per sample")
		// Self-enrolment. A DaemonSet or a scratch/distroless image has no
		// shell to run "enroll then run" in, and the image deliberately ships
		// none — so run enrols itself when it has a token and no identity yet.
		// Existing identity always wins, so a restart never burns another
		// token use or leaves a duplicate machine record behind.
		token := fs.String("token", os.Getenv("FLEET_TOKEN"), "enrollment token; used only if not already enrolled (or FLEET_TOKEN)")
		fp := fs.String("fingerprint", os.Getenv("FLEET_CA_FINGERPRINT"), "sha256 fingerprint of the control-plane CA (or FLEET_CA_FINGERPRINT)")
		name := fs.String("name", hostnameOr(""), "machine display name used when self-enrolling")
		_ = fs.Parse(os.Args[2:])
		if *server == "" {
			fmt.Fprintln(os.Stderr, "run requires --server")
			os.Exit(2)
		}
		a := &agent.Agent{ServerURL: *server, DataDir: *data, Log: log, Heartbeat: *hb, MetricsEvery: *me, TopProcesses: *tp}
		if !a.Enrolled() {
			if *token == "" {
				log.Error("not enrolled; run `fleet-agent enroll` first, or pass --token to self-enroll")
				os.Exit(1)
			}
			log.Info("no identity yet; self-enrolling", "name", *name)
			if err := a.Enroll(*token, *name, *fp); err != nil {
				log.Error("self-enroll failed", "err", err)
				os.Exit(1)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("agent exited", "err", err)
			os.Exit(1)
		}

	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fleet-agent <enroll|run> [flags]")
	os.Exit(2)
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return def
}
