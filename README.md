# fleetcore

A minimal, modular fleet-management core for Linux servers — the thin foundation of an Arc-style control plane. Stdlib-only Go, two static binaries, zero external dependencies.

```
┌─────────────┐  admin REST (bearer)  ┌──────────────────────────────┐
│  operator    ├──────────────────────►  fleet-server                │
└─────────────┘                       │  ├─ CA (agent mTLS identity) │
                                      │  ├─ Store (interface)        │
        outbound mTLS only            │  ├─ Bus   (interface)        │
┌─────────────┐  enroll/heartbeat/SSE │  └─ ed25519 module signing   │
│ fleet-agent  ├──────────────────────►                              │
│ (systemd)    │◄─ desired state ─────┤                              │
└─────────────┘                       └──────────────────────────────┘
```

Design rules the core enforces:

- **Outbound-only agents.** Works behind NAT/firewalls; the agent holds one SSE stream and dials everything.
- **Declarative reconcile, never imperative commands.** Operators set a machine's `DesiredState`; agents converge toward it. Offline tolerance and retries fall out for free.
- **Identity = mTLS.** Client certs carry `CN=<machine-id>`, `OU=<tenant-id>`; every request is attributed from the cert, never the body. Tenants are isolation boundaries from day one.
- **Dumb agent, smart modules.** The agent contains no feature logic. It fetches ed25519-signed payloads and executes them. All capability growth happens in modules.

## Layout

```
cmd/fleet-server     control plane entrypoint
cmd/fleet-agent      agent entrypoint (enroll | run)
internal/api         wire types — the contract
internal/ca          root CA, agent cert issuance, module signing
internal/store       Store interface + FileStore + TursoStore (Hrana HTTP)
internal/bus         Bus interface + in-proc impl   (NATS drops in)
internal/server      agent-facing mTLS API + admin REST API
internal/agent       runtime: enroll, heartbeat, SSE reconcile, module exec
packaging/           systemd units
scripts/e2e.sh       end-to-end smoke test
```

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/ ./cmd/...
# fleet-agent ≈ 5 MB static; cross-compile with GOARCH=arm64 etc.
```

## Storage

Default is a local JSON file (dev/single-node). For hosted deployments point the server at Turso — no driver, no cgo; the store speaks the Hrana HTTP pipeline (`/v2/pipeline`) directly, so any sqld-compatible endpoint works (Turso cloud or self-hosted sqld):

```sh
fleet-server --db-url https://<db>-<org>.turso.io --db-token "$(turso db tokens create <db>)"
# or env: FLEET_DB_URL / FLEET_DB_TOKEN
```

Offline tests run against `scripts/hrana_emulator.py` (real SQLite behind the same wire protocol):

```sh
python3 scripts/hrana_emulator.py 9090 &
HRANA_URL=http://127.0.0.1:9090 go test ./internal/store/
scripts/e2e_turso.sh   # full stack incl. control-plane restart persistence
```

Note: enrollment token consumption is a single `UPDATE ... RETURNING`, so single-use semantics hold under concurrency. CA/signing keys stay on the server's disk, not in the DB — replicate `<data>/ca` when running multiple control-plane instances.

## Deploying agents fleet-wide

The control plane serves its own installer when started with `--binaries <dir>` (containing `fleet-agent-linux-{amd64,arm64}`). Onboarding any machine is then one command:

```sh
curl -sk https://cp.example.com:8443/install.sh | FLEET_TOKEN=<token> sh
```

The generated script pins the CA fingerprint and per-arch SHA-256 checksums, detects the architecture, installs to `/usr/local/bin`, enrolls (idempotent — keeps an existing identity), writes and enables the systemd unit (`FLEET_METRICS=1s` to set the live interval; `FLEET_NAME` to name the machine), and falls back to printing manual instructions where systemd is absent.

Enrollment tokens support fleets: `POST /admin/tokens {"tenant_id":..., "max_uses":50, "ttl_sec":604800, "labels":{"region":"sg"}}` mints a token 50 machines can enroll with over a week, each landing pre-labeled (`max_uses: -1` = unlimited, default remains single-use). Mint one token per region/site/role and bake the one-liner into Ansible, cloud-init user-data, autoscaling launch templates, or RunPod startup scripts.

## Quickstart

```sh
# 1. control plane
FLEET_ADMIN_TOKEN=$(openssl rand -hex 24) ./bin/fleet-server \
  --data /var/lib/fleet-server --agent-addr :8443 \
  --admin-addr 127.0.0.1:8080 --san cp.example.com
# note the ca_sha256 fingerprint it logs at startup

# 2. tenant + one-time enroll token
auth="Authorization: Bearer $FLEET_ADMIN_TOKEN"
curl -H "$auth" -d '{"name":"acme"}' localhost:8080/admin/tenants
curl -H "$auth" -d '{"tenant_id":"<id>"}' localhost:8080/admin/tokens

# 3. on each server
fleet-agent enroll --server https://cp.example.com:8443 \
  --token <token> --fingerprint <ca_sha256>
systemctl enable --now fleet-agent   # packaging/fleet-agent.service

# 4. ship a module (any executable payload)
payload=$(base64 -w0 apply.sh)
curl -H "$auth" -d "{\"name\":\"baseline\",\"version\":\"1.0.0\",\"payload\":\"$payload\"}" \
  localhost:8080/admin/modules
curl -X PUT -H "$auth" \
  -d '{"modules":[{"name":"baseline","version":"1.0.0","config":{"ssh_port":"22"}}]}' \
  localhost:8080/admin/machines/<machine-id>/desired
```

The agent receives the new desired state on its stream within milliseconds, downloads the payload, verifies the ed25519 signature against the key pinned at enrollment, executes it with `FLEET_CFG_*` env vars, and reports `applied`/`failed` with captured output.

## API surface

Agent (mTLS, `:8443`): `POST /v1/enroll` · `POST /v1/heartbeat` · `GET /v1/stream` (SSE) · `GET /v1/module` · `POST /v1/status`

Admin (bearer, `127.0.0.1:8080`): `POST|GET /admin/tenants` · `POST /admin/tokens` (with optional `labels`) · `GET /admin/machines[/{id}]` · `PUT /admin/machines/{id}/labels` · `PUT /admin/machines/{id}/desired` (override) · `POST|GET|PATCH|DELETE /admin/groups[/{id}]` · `GET|POST|DELETE /admin/groups/{id}/members[/{machine_id}]` · `POST|GET /admin/modules` · `GET /metrics`

## Grouping model

Ownership is a hierarchy; placement is a label. The data model is flat — `tenant → machines` — and machines carry a `labels` map (`region=sg`, `role=gpu`, `env=prod`). Enrollment tokens can embed labels, so a token minted per region/site stamps machines correctly at enrollment.

A **group** clusters machines for management: membership = explicit members ∪ machines whose labels match the group's `selector` (all pairs must match; empty selector = explicit-only) ∪ everyone, if `match_all` is set. Every tenant is created with a built-in `all` group (`match_all: true`) — attach baseline modules there and every machine, present and future, receives them; it's an ordinary group otherwise (editable, deletable if you don't want a default). Groups carry the module list. A machine's effective `DesiredState` is **computed**: union of its groups' modules (groups applied in name order, later wins on conflicts), then the machine's `override` replaces same-named modules. Recompute triggers on group create/update/delete, membership change, label change, override change, and enrollment — revision bumps and streams wake only when the module set actually changed. Agents are unaware of all of this; they still reconcile a plain `DesiredState`.

`scripts/e2e_groups.sh` verifies: selector membership applies at enrollment, unlabeled machines are untouched, explicit member add applies, override beats group.

## Prometheus

`GET /metrics` on the admin listener exposes fleet facts in exposition format: `fleet_machine_info`, `fleet_machine_last_seen_timestamp_seconds`, uptime/packages/processes gauges, `fleet_machine_gpu_info` (PCI display-class scan; NVIDIA models resolved via driver procfs), `fleet_machine_service_info` (recognised server processes, each tagged with a `category` label: database, kubernetes, container, web, messaging, ai, monitoring, system), and `fleet_module_applied`. Scrape with bearer auth:

```yaml
scrape_configs:
  - job_name: fleetcore
    authorization: { credentials: <admin-token> }
    static_configs: [{ targets: ["cp.internal:8080"] }]
```

Alerting on dead machines is then `time() - fleet_machine_last_seen_timestamp_seconds > 120`.

## Live metrics

The agent additionally pushes a live sample every `--metrics-interval` (default 10s, 0 disables): CPU percent, load1, memory, root-disk usage, and network rx/tx rates — all read from `/proc` and `statfs`, no subprocesses. The control plane keeps only the latest sample per machine **in memory** (durable series are Prometheus's job) and exposes it two ways:

- `GET /admin/machines/{id}/live` — SSE stream of samples as they arrive, for real-time dashboards.
- `GET /dashboard` — built-in netdata-style live dashboard (single static page, no external assets). Rolling CPU / memory / network / disk charts, a top-processes table (htop-style), and GPU cards when a collector is running. Run agents with `--metrics-interval 1s` for per-second streaming.
- `GET /metrics/alerts` — a ready-to-load Prometheus alerting rules file (9 rules: machine down, disk almost/critically full, memory high, CPU saturated, security updates pending, reboot required, module failed, GPU overheating). Drop into your Prometheus `rule_files`.

### Per-process, GPU, and alerts

Each live sample now carries the top-N processes by CPU (`--top-processes`, default 5), read from `/proc/[pid]/stat` deltas — exposed as `fleet_live_process_cpu_percent` and `fleet_live_process_memory_bytes` labeled by pid/comm. GPU utilization is collected by a module (`examples/gpu-collector/collect.sh`) that polls `nvidia-smi` and pushes through the agent (keeping NVML out of the core); deploy it to your `gpu-fleet` group and get `fleet_live_gpu_utilization_percent`, `fleet_live_gpu_memory_used_bytes`, `fleet_live_gpu_temp_celsius`, and `fleet_live_gpu_power_watts`. The dashboard shows all three.
- `fleet_live_*` gauges on `/metrics` (cpu_percent, load1, memory/disk used+total bytes, net rx/tx Bps), emitted only while a machine's sample is fresh (<30s), so scrapes reflect reality even when agents die.

The registry is per-control-plane-instance; the NATS bus upgrade carries cross-instance fan-out when needed. Deep host metrics (per-process, GPU utilization via NVML/DCGM) remain a collector-module concern pushing `remote_write` — machines are behind NAT and cannot be scraped directly.

## Module contract (v0.1)

A module is an executable payload + version. Execution model is one-shot apply per `(name, version, config)` tuple, remembered in `applied.json`. Environment: `FLEET_MODULE_NAME`, `FLEET_MODULE_VERSION`, `FLEET_MACHINE_ID`, `FLEET_CFG_<KEY>`. Exit 0 = applied. Idempotency is the module's responsibility — same rule as any config management.

## Where the seams are (what to swap as you scale)

| Concern | v0.1 | Drop-in later |
|---|---|---|
| Persistence | JSON `FileStore` or Turso/libSQL (`--db-url`) | any sqld-compatible endpoint; other DBs behind `store.Store` |
| Fan-out | in-proc `bus.InProc` | NATS behind `bus.Bus` (multi-instance CP) |
| Admin authn | static bearer token | OIDC in front of the admin listener |
| Server TLS | CA-issued cert | public cert (LE) on the agent listener; agents still pin your CA for client auth |
| Module exec | plain child process | `systemd-run` scoped units / cgroup limits; long-running supervisor module |
| Bootstrap trust | `--fingerprint` (or TOFU) | TPM/SPIFFE attestation |

## Package / patch management

Split across the two halves the architecture already implies:

- **Detection (in the agent, reported via inventory):** every heartbeat carries an `Updates` summary — pending package count, security-update count, up to 50 package names, and the Debian reboot-required flag — for both apt and dnf. Refreshed at most every 6h (package queries are slow), cached in between. Surfaced as Prometheus gauges `fleet_machine_updates_pending`, `fleet_machine_updates_security`, and `fleet_machine_reboot_required`, so "how many boxes have unpatched security holes" and "which need a reboot" are one query. Alert example: `sum(fleet_machine_updates_security) by (name) > 0`.
- **Action (a signed module):** `examples/patch-module/patch.sh` performs the upgrade, driven by `FLEET_CFG`: `mode=security|all`, `reboot=auto|never`, `refresh=yes|no`. Push it to a group (e.g. the built-in `all` group for fleet-wide patching, or a canary group first) and every member applies on reconcile, reporting applied/failed with captured output. Idempotent — re-running when nothing is pending is a no-op.

The division is deliberate: detection is cheap and universal so it lives in the core; applying updates is policy (which machines, when, security-only vs all, reboot or not) so it's a module you schedule and target per group.

## Kubernetes awareness

Inventory reports a `kubernetes` role derived from running processes — `control-plane`, `worker`, `k3s-server`, or `k3s-agent` (empty for non-K8s nodes), plus the underlying container/database/kubernetes services in the categorized service list (each `Service` carries a `category`, so `postgres` reports as `database`, `kubelet` as `kubernetes`, `nginx` as `web`, and so on — no more flat undifferentiated list). Exposed as `fleet_machine_kubernetes_info{role=...}`. This is detection only — it tells you which machines run Kubernetes and in what role, so you can group and target them (e.g. a GitOps module onto `control-plane` nodes). Managing the clusters themselves stays delegated to Flux/ArgoCD/OCM via a future module, per the original plan.

## Deliberate non-goals of the core

Config-management DSLs, package management, remote shell, metrics shipping, Kubernetes — all of these are modules. The core stays: identity, transport, inventory, desired-state, signed execution.
