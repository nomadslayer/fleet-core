# fleetcore — deployment guide

Two parts: **(1)** stand up the control plane with Docker Compose, **(2)** register machines. Everything here has been exercised end-to-end; commands are copy-paste ready once you substitute your hostname.

---

## Part 1 — Control plane (Docker Compose)

The compose stack runs three long-lived containers: `sqld` (self-hosted libSQL, Turso-compatible), `control-plane` (fleetcore), and `prometheus` (scrapes the control plane, evaluates the bundled alert rules). The control plane image also builds and carries the agent binaries, so it serves the installer itself.

### 1.1 Prerequisites

- A host with Docker + Docker Compose, reachable by your machines.
- A DNS name pointing at it (e.g. `cp.example.com`). Agents connect here and pin the CA, so the name must be stable.
- Port **8443** open to wherever your machines live (the agent API). Ports **8080** (admin) and **9090** (Prometheus) stay private — both are bound to `127.0.0.1`.

### 1.2 Start it

```sh
cd deploy
export FLEET_ADMIN_TOKEN=$(openssl rand -hex 24)   # save this — it's your admin credential
export FLEET_SAN=cp.example.com                     # your public hostname
docker compose up -d --build
```

Both variables are required by *every* `docker compose` command in this directory, `ps` and `logs` included — a fresh shell without them fails with `required variable FLEET_ADMIN_TOKEN is missing a value`. For a host you'll come back to, write them to `deploy/.env` (compose reads it automatically) and lock it down:

```sh
cat > .env <<EOF
FLEET_ADMIN_TOKEN=$FLEET_ADMIN_TOKEN
FLEET_SAN=$FLEET_SAN
EOF
chmod 600 .env      # contains your admin credential — never commit it
```

Confirm it's healthy and grab the CA fingerprint (needed for manual enrollments; the installer embeds it automatically):

```sh
docker compose logs control-plane | grep ca_sha256
# level=INFO msg="certificate authority ready" ca_sha256=<64 hex chars>
docker compose exec control-plane wget -qO- http://127.0.0.1:8080/healthz   # -> ok
```

### 1.3 Reach the admin API

The admin port is bound to `127.0.0.1` on the host — never expose it raw. From your laptop:

```sh
ssh -N -L 8080:127.0.0.1:8080 you@cp.example.com    # tunnel
export CP=http://127.0.0.1:8080
export AUTH="Authorization: Bearer $FLEET_ADMIN_TOKEN"
curl -s -H "$AUTH" $CP/admin/tenants
```

For a permanent setup, put the admin API behind your reverse proxy (Caddy/Traefik/nginx) with its own TLS and, ideally, OIDC — the built-in bearer token is a single shared secret, fine for one operator, not for a team.

### 1.4 What to back up

- **`cp-data` volume** — holds the CA and module-signing keys. Losing it means every agent's identity is orphaned and every module signature invalid. Back it up.
- **`sqld-data` volume** — all fleet state (tenants, machines, groups, tokens). For off-host durability, enable bottomless S3 replication (commented block in `docker-compose.yml`) or snapshot the volume on a timer.

### 1.5 Live dashboard

Through the same tunnel: open `http://127.0.0.1:8080/dashboard`, paste the admin token, pick a machine, Stream.

### 1.6 Prometheus

The stack includes Prometheus, scraping the admin listener's `/metrics` every 15s over the internal network and evaluating the nine bundled `fleet_*` alert rules. The UI is on `127.0.0.1:9090` — tunnel to it the same way as the admin API.

The scrape needs the admin bearer token. Rather than putting it in `prometheus.yml`, a `prom-init` container writes `$FLEET_ADMIN_TOKEN` into a volume as `/run/fleet/admin_token` on every `up`, and the scrape config reads it via `credentials_file`. Rotating the token is `docker compose up -d --force-recreate prom-init prometheus`.

```sh
# is the control plane being scraped?
curl -s 'http://127.0.0.1:9090/api/v1/targets?state=active' \
  | python3 -c 'import sys,json;[print(t["labels"]["job"],t["health"],t["lastError"]) for t in json.load(sys.stdin)["data"]["activeTargets"]]'

# what is firing right now?
curl -s http://127.0.0.1:9090/api/v1/alerts \
  | python3 -c 'import sys,json;[print(a["state"],a["labels"]["alertname"],a["labels"].get("name")) for a in json.load(sys.stdin)["data"]["alerts"]]'
```

Alert rules live in `prometheus/rules/fleetcore.yml`, a copy of what the control plane serves at `GET /metrics/alerts`. Refresh after a fleetcore upgrade:

```sh
curl -s -H "$AUTH" $CP/metrics/alerts > prometheus/rules/fleetcore.yml
docker compose exec prometheus kill -HUP 1
```

Retention is 15 days on the `prom-data` volume. This is fleet state, not deep host telemetry — for per-process and GPU series, run a collector module that pushes `remote_write` directly (machines are behind NAT and can't be scraped).

Point Grafana at `http://prometheus:9090` if you attach it to the `internal` network; add Alertmanager under `alerting:` in `prometheus/prometheus.yml` when you want the alerts to actually page someone.

---

## Part 2 — Registering machines

### 2.0 What a machine needs

Agents are outbound-only. A machine can sit behind NAT, in another cloud, on a home connection — it dials the control plane, never the reverse.

| Requirement | Detail |
|---|---|
| Network | Outbound TCP **443/8443** to the control plane. No inbound ports, no VPN, no public IP. |
| OS | Linux, `amd64` or `arm64`. The binary is static (`CGO_ENABLED=0`) — works on glibc and musl alike. |
| Privileges | root for the one-line installer (writes `/usr/local/bin`, `/etc/systemd/system`, `/var/lib/fleet-agent`). |
| Tools | `curl` and `sha256sum` for the installer. Neither is needed afterwards. |
| Footprint | One process, `MemoryMax=128M` in the shipped unit. Reads `/proc` and `statfs` directly — spawns no subprocesses. |

**The one thing that will bite you: `FLEET_SAN`.** Agents verify the control plane's certificate against the name they dial, so whatever hostname you put in the install command *must* be in the server cert's SAN list. `FLEET_SAN` takes a comma-separated list — set every name and IP agents might use, then restart:

```sh
export FLEET_SAN="cp.example.com,cp.internal,203.0.113.10"
docker compose up -d
```

Changing it re-issues the server certificate. Agents are unaffected — they pin the **CA**, not the leaf — so no re-enrollment is needed.

### 2.1 One-time setup per tenant

A tenant is an isolation boundary (a customer, an environment). Create one, then mint an enrollment token. Labels on the token are stamped onto every machine that uses it — this is how machines land in the right groups automatically.

```sh
# create tenant
TENANT=$(curl -s -H "$AUTH" -d '{"name":"pantheonlab"}' $CP/admin/tenants \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# mint a fleet token: up to 100 machines, valid 7 days, labeled by site+role
TOKEN=$(curl -s -H "$AUTH" -d "{
  \"tenant_id\":\"$TENANT\",
  \"max_uses\":100,
  \"ttl_sec\":604800,
  \"labels\":{\"region\":\"hk\",\"role\":\"gpu\"}
}" $CP/admin/tokens | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
echo $TOKEN
```

Token options: `max_uses` — `1` (default) single machine, `N` up to N, `-1` unlimited (use with a TTL for autoscaling). `ttl_sec` — `0` = 24h default, `-1` = never expires.

### 2.2 Install on a machine (the one command)

On any Linux box that can reach the control plane:

```sh
curl -sk https://cp.example.com:8443/install.sh | FLEET_TOKEN=<token> sh
```

That downloads the right binary for the CPU arch, verifies its checksum, enrolls (CA fingerprint pinned), installs a systemd unit, and starts the agent. Useful env vars:

- `FLEET_NAME=web-01` — machine display name (defaults to hostname)
- `FLEET_METRICS=1s` — live-metrics interval (default `10s`; `0` disables)
- `FLEET_NO_SYSTEMD=1` — skip unit setup, just install + enroll

Re-running upgrades the binary but keeps the existing identity.

### 2.3 Confirming the machine was captured

Three independent places to look, in the order they light up.

**1 — the machine itself.** Enrollment logs the ID it was issued:

```
level=INFO msg=enrolled machine=6a042ab3f26278ee6ef4fb09a0940202 tenant=c3ee077bf...
level=INFO msg="stream connected"
level=INFO msg=reconciling revision=1 modules=0
```

`stream connected` is the one that matters — the agent is holding the SSE reconcile channel open and will act on desired-state changes within a second.

**2 — the admin API.** Inventory arrives with the first heartbeat, typically within a few seconds:

```sh
curl -s -H "$AUTH" "$CP/admin/machines?tenant=$TENANT" \
  | python3 -c 'import sys,json;[print(m["name"],m["labels"],m["inventory"]["os"],m["inventory"]["arch"]) for m in json.load(sys.stdin)]'
# agent-lab-01 {'region': 'local', 'role': 'test'} alpine arm64
```

**3 — Prometheus.** Within one scrape interval (15s) the machine becomes a time series:

```sh
curl -s --data-urlencode 'query=fleet_machine_info' http://127.0.0.1:9090/api/v1/query \
  | python3 -c 'import sys,json;[print(r["metric"]["name"],r["metric"]["os"],r["metric"]["arch"]) for r in json.load(sys.stdin)["data"]["result"]]'

# seconds since each machine last checked in — the health signal
curl -s --data-urlencode 'query=time()-fleet_machine_last_seen_timestamp_seconds' \
  http://127.0.0.1:9090/api/v1/query \
  | python3 -c 'import sys,json;[print(r["metric"]["name"],round(float(r["value"][1]),1),"s ago") for r in json.load(sys.stdin)["data"]["result"]]'
```

If a machine stops reporting, `FleetMachineDown` fires about three minutes later (120s stale + `for: 1m`) and its `fleet_live_*` series stop being emitted after 30s, so live panels go empty rather than flatlining on a stale value.

**If it doesn't show up**, work down the list:

| Symptom on the machine | Cause |
|---|---|
| `x509: certificate is valid for ..., not cp.example.com` | The name you dialed isn't in `FLEET_SAN`. See 2.0. |
| `connection refused` / timeout on 8443 | Firewall or security group; agent API not reachable. |
| `403 invalid token` | Token expired (`ttl_sec`) or `max_uses` exhausted. Mint a new one. |
| Enrolled once, now `403 unknown machine` | The machine was deleted from the admin API — that *is* the revocation path. Re-enroll. |
| `no binary published for <arch>` | Not amd64/arm64. |

### 2.4 Rolling it out to a whole fleet

Same one command, driven by whatever you already use. Mint one token per site/role, then:

**Ansible**

```yaml
- hosts: all
  become: true
  tasks:
    - shell: |
        curl -sk https://cp.example.com:8443/install.sh | FLEET_TOKEN={{ fleet_token }} sh
      args: {creates: /usr/local/bin/fleet-agent}
```

**Cloud-init / AWS user-data / Azure custom-data** (self-enrolling autoscaling nodes — use an unlimited token with a TTL):

```yaml
#cloud-config
runcmd:
  - curl -sk https://cp.example.com:8443/install.sh | FLEET_TOKEN=<token> FLEET_METRICS=5s sh
```

**RunPod / GPU nodes** — put the same line in the pod startup script with a `role=gpu` token; the DCGM/metrics module attached to your `gpu-fleet` group then applies automatically.

**Golden image** — bake the binary + unit but NOT `/var/lib/fleet-agent`; enrollment on first boot gives each clone a unique identity.

**No systemd** (containers, minimal distros, Alpine/OpenRC) — `FLEET_NO_SYSTEMD=1` installs and enrolls but doesn't set up a unit; supervise it yourself:

```sh
curl -sk https://cp.example.com:8443/install.sh | FLEET_TOKEN=<token> FLEET_NO_SYSTEMD=1 sh
/usr/local/bin/fleet-agent run --server https://cp.example.com:8443 --metrics-interval 10s
```

Keep `/var/lib/fleet-agent` on persistent storage — it holds the machine's private key and certificate. A container that loses it re-enrolls as a *new* machine on next start (burning another token use and leaving a stale entry to clean up).

### 2.5 Rehearsing on one host first

Before touching real machines, run the whole enrollment path locally — two throwaway agents against the compose stack, no VMs:

```sh
docker run -d --name agent-test-01 --hostname agent-test-01 \
  --network deploy_default -e FLEET_TOKEN="$TOKEN" alpine:3.20 sh -c '
    set -e; apk add -q curl
    curl -sk https://control-plane:8443/install.sh | FLEET_NAME=agent-test-01 FLEET_NO_SYSTEMD=1 sh
    exec /usr/local/bin/fleet-agent run --server https://control-plane:8443 --metrics-interval 2s'

docker logs -f agent-test-01     # expect: enrolled -> stream connected -> reconciling
```

This requires `control-plane` in `FLEET_SAN` (that's the name the container dials). Verify with the three checks in 2.3, then tear down — `docker rm -f agent-test-01` and `curl -s -X DELETE -H "$AUTH" $CP/admin/machines/<id>`.

### 2.6 Managing machines after enrollment

```sh
# group machines (baseline module reaches every machine via the built-in "all" group)
curl -s -H "$AUTH" -d "{\"tenant_id\":\"$TENANT\",\"name\":\"gpu-fleet\",
  \"selector\":{\"role\":\"gpu\"},\"modules\":[{\"name\":\"dcgm\",\"version\":\"1.0.0\"}]}" \
  $CP/admin/groups

# relabel a machine (triggers recompute of its desired state)
curl -s -X PUT -H "$AUTH" -d '{"labels":{"region":"hk","role":"gpu","env":"prod"}}' \
  $CP/admin/machines/<machine-id>/labels

# decommission (also revokes the cert — the agent's next call gets 403)
curl -s -X DELETE -H "$AUTH" $CP/admin/machines/<machine-id>
```

On the machine itself:

```sh
systemctl status fleet-agent
journalctl -u fleet-agent -f          # watch heartbeats, reconciles, metrics
systemctl stop fleet-agent            # pause without decommissioning
```

To fully remove: `systemctl disable --now fleet-agent && rm -rf /var/lib/fleet-agent /usr/local/bin/fleet-agent /etc/systemd/system/fleet-agent.service`, then delete it from the admin API.

---

## Security checklist before production

- [ ] Admin API not exposed publicly (tunnel or authenticated reverse proxy only).
- [ ] `FLEET_ADMIN_TOKEN` stored in a secret manager, not shell history.
- [ ] `cp-data` volume backed up (CA + signing keys).
- [ ] `sqld-data` backed up or bottomless S3 enabled.
- [ ] Enrollment tokens scoped with `ttl_sec`; unlimited tokens rotated per rollout wave.
- [ ] For high-trust sites, distribute `install.sh` out-of-band instead of `curl -sk` (the first fetch is trust-on-first-use).
- [ ] sqld auth (`SQLD_AUTH_JWT_KEY`) enabled if the DB is ever reachable beyond the compose network.
