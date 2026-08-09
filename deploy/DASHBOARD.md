# The built-in dashboard

`GET /dashboard` on the admin listener. One self-contained HTML page — no
build step, no external assets, no CDN. It is served unauthenticated because
it contains no data: every byte it displays is fetched by the browser using
an admin token you paste in, and that token is what the API checks.

Reach it the same way you reach the rest of the admin API:

```sh
kubectl -n <ns> port-forward svc/<release>-fleetcore-admin 8080   # Kubernetes
ssh -N -L 8080:127.0.0.1:8080 you@cp.example.com                  # VM / compose
open http://127.0.0.1:8080/dashboard
```

---

## How data reaches the page

Three separate channels, which is worth understanding because the UI shows
each of them differently.

| Channel | Rate | Carries | Endpoint |
|---|---|---|---|
| **Heartbeat** | 30s default | `last_seen` + full inventory, written to the store | agent → `POST /v1/heartbeat` |
| **Live metrics** | 10s default (500ms in the lab) | CPU/mem/disk/net/pods/processes, **in memory only** | agent → `POST /v1/metrics` |
| **SSE stream** | event-driven | one frame per agent push, for the open machine | UI ← `GET /admin/machines/{id}/live` |

The UI polls `/admin/machines` and `/admin/live` every 2s for the whole
fleet, and additionally opens an SSE stream for whichever machine you have
open. Because the poll runs for **every** machine all the time, a machine
already has chart history before you ever click it — opening a machine
resumes rather than starting from an empty window.

**A machine can be "live" without a recent heartbeat.** Live metric pushes
deliberately skip the store (a write per machine every 500ms would be a write
storm), so `last_seen` advances only on the 30s heartbeat. The UI therefore
shows `live · 2s` when samples are streaming and only falls back to a
heartbeat age when they are not. Both are shown separately in the detail
header so the two are never conflated.

Chart history is persisted to `localStorage` and restored on reload, because
the control plane keeps only the newest sample per machine — durable series
are Prometheus's job, so there is nothing server-side to re-fetch. Anything
older than 120s is discarded so a tab reopened tomorrow does not show stale
data as if it were current.

---

## Global chrome (visible in both views)

**Theme control** — cycles Auto → Light → Dark, persisted. *Auto* follows
`prefers-color-scheme` and re-reads canvas colours when the OS flips while
the page is open. Explicit choices override the system in both directions.

**Fleet health badge** — `N machines online`, or red `N offline: name` when
any machine is down. Deliberately global: it is visible while you are drilled
into a *different* machine, so a box dropping is never missed. Click it to
return to the fleet view.

**Document title** — becomes `(1 down) fleetcore`, so a background tab shows
the fleet state without being focused.

**Offline event banners** — when a machine transitions up→down or down→up, a
banner appears and *persists until dismissed*. A machine that dropped while
you were reading another one is still there when you come back.

**Sign out** — clears the stored token and all cached state.

---

## Fleet overview

### Summary strip
`machines` · `online` · `offline` (red when non-zero) · `avg cpu` across
machines with live samples · `memory` summed used/total.

### Run command on a group
A textarea, a group picker, and a label-selector box.

- Leave the selector empty → the command targets the **selected group**.
- Fill it with `role=gpu region=hk` → the command targets **every machine
  matching all of those labels**, across groups. This exists because a group
  must be created in advance; a selector matches labels directly, so an
  intersection that has no group still works. A selector that matches nothing
  returns 404 and the payload is discarded rather than orphaned.

Commands run as root via `/bin/sh` with working directory `/`. Ctrl/Cmd+Enter
submits. The reply reports how many machines matched.

### Filter
Free-text across name, machine id, primary IP, all interface IPs, hostname,
OS, version, arch, Kubernetes role, and `key=value` labels. Multiple
whitespace-separated terms must **all** match, so `gpu hk` narrows rather
than widens. Shows `N of M shown` when filtering.

### Machine cards
Every card has the same fixed vertical rhythm — the OS line is clamped to one
line and the label chips are pinned to the bottom — so bars and sparklines
line up across the grid regardless of content length.

| Element | Meaning |
|---|---|
| Status dot | green = up, red = down (no heartbeat for 120s **and** no fresh live sample) |
| Name | machine display name, set at enrolment |
| Right-hand label | `live · 2s` (green) while samples stream, otherwise heartbeat age, or `down 5m` |
| **IP** | the address on the interface holding the default route; `+N more` when the machine is multi-homed |
| Sub-line | OS + version · arch · cores · uptime · k8s role |
| cpu / mem / disk bars | colour-graded — green < 70%, amber < 90%, red above |
| Sparkline | rolling CPU history, spanning whatever history exists |
| Chips | labels stamped at enrolment or set later |
| **Remove** | always present, brighter on hover and on offline cards |

Cards update **in place** rather than being rebuilt, so bar widths animate
and the page reads as live rather than repainting.

**Remove** deletes the machine record and is also the revocation path —
identity is re-checked against the store on every request, so the certificate
stops working immediately. The confirmation says so, and warns that an agent
still holding an enrolment token will re-enrol as a *new* machine.

---

## Machine detail

Click any card. Order is deliberate: **what it is doing now**, then **what
you can do to it**, then **what it is**.

### Header
Name · online/OFFLINE · `heartbeat 8s ago` · `live sample 0s ago` · machine
id. Below it, **group membership chips** stating *why* the machine is a
member — `every machine in the tenant` (match-all), `labels role=gpu`
(selector, showing the actual labels), or `added directly` (explicit). Group
membership is the union of three independent rules, which is why it needs its
own lookup rather than being readable off the group records.

### Live charts
CPU (with load average), memory, network (rx and tx overlaid), disk. Rolling
120-point window, hand-drawn canvas, fed by the SSE stream. Colours are read
from the active theme at draw time.

### Run command on this machine
Queues into the machine's **override**, which wins over a group command of
the same name.

### Pending
Only appears when something is queued. A command is *pending* when it is in
desired state but the machine has not reported it yet — i.e. the agent has
not pulled it. Everything is pull-based, which is what lets NAT'd machines
work with no inbound access.

### Command history
Newest first, capped (`--max-command-history`, default 20 per target; the
header states the live value). Each row shows the state badge
(`applied` / `failed` / `queued`), the generated command id, the script as
you typed it, the captured stdout/stderr, and the time.

- **Re-run** creates a *new* command with the same script. Modules execute
  once per `(name, version, config)`, so re-issuing the same id would be a
  no-op the agent skips as already converged.
- **Cancel** removes a queued command; on a finished one it removes it from
  history.

### Processes
`N running · NNNMiB resident total` — totals across the **whole** process
table, not just the rows shown. The list is a union of top-N by CPU, top-N by
RSS, and every recognised service, because a CPU-ranked list alone cannot
answer "where is my memory going" (a 40GiB process idling at 0% never
appears) and cannot confirm a daemon is running at all.

Columns: pid, process (`/proc/<pid>/comm`, 15-char kernel limit), **service**
(canonical name when recognised), cpu%, mem.

### Network interfaces
`N total · M physical`. Every interface with addresses, MAC, link speed and
live rx/tx. The default-route interface is tagged `primary`; veth, cni,
flannel, docker, bridge and tunnel devices are tagged `virtual` so a node with
one veth per pod stays readable. Down interfaces are marked.

### Inventory
hostname · primary ip · interface count · os + version · kernel · arch · cpu
cores · uptime · packages · processes · agent version · **kubernetes role** ·
desired revision · pending/security updates · detected services.

### Modules
Assigned modules with version and state. Ad-hoc commands are filtered out
here — they have their own panel.

---

## Machines running Kubernetes

A Kubernetes node is an ordinary machine plus three additions. Nothing about
enrolment or transport differs.

### Role detection
`inventory.kubernetes` is derived purely from which processes are running —
no kubectl, no API calls, no kubeconfig:

| Detected | Reported role |
|---|---|
| `k3s` with `server` in argv | `k3s-server` |
| `k3s` with `agent` in argv | `k3s-agent` |
| `kube-apiserver` | `control-plane` |
| `kubelet` alone | `worker` |
| none of the above | *(empty — not a K8s node)* |

k3s re-execs itself and rewrites its process title, so it appears as comm
`k3s` *or* comm `exe` with argv collapsed into a single `"k3s server"`
string. Both forms are handled; splitting on NUL alone finds nothing.

Shown on the card sub-line, in the inventory panel, and exported as
`fleet_machine_kubernetes_info`.

### Pods panel
Appears **only** on machines reporting pods. Header:
`N pods · X% cpu · YMiB · via kubelet|cgroup`.

Columns: namespace, pod, containers, cpu (percent of one core), memory
(working set). Collected two ways, preferring the first:

**kubelet Summary API** — `https://127.0.0.1:10250/stats/summary`. Gives
namespace, pod name, and **per-container** figures straight from cAdvisor.
Needs a bearer token with `get` on `nodes/stats`; the Helm chart creates the
ServiceAccount and RBAC, or set `FLEET_KUBELET_TOKEN`. Falls back to the
read-only port 10255 where it is still enabled.

**cgroup fallback** — no credentials, no network, works on any CRI. Walks the
kubepods tree and reads `cpu.stat` / `memory.current`, subtracting
`inactive_file` so the number matches `kubectl top` rather than counting
reclaimable page cache. Three tree layouts are handled: systemd driver
(kubeadm default, `kubepods.slice/…-pod<uid>.slice` with underscore-escaped
UIDs), cgroupfs driver (k3s, `kubepods/burstable/pod<uid>`), and cgroup v1
(one tree per controller, values split across them). Pod **names** come from
`/var/log/pods` (`<namespace>_<name>_<uid>`), the only credential-free source
of pod identity on a node; without it you get a UID and no name.

Both paths were cross-checked against a live k3s node and produced identical
memory figures.

### Interfaces on a K8s node
Typically 15–20 entries: the real NIC, plus `cni0`, `flannel.1`, and a veth
per pod. All are reported; the virtual ones are tagged so the table stays
scannable and `primary` still identifies the node's real address.

### Processes on a K8s node
`kubelet`, `containerd`, `k3s-server`, `kube-apiserver`, `etcd`, `coredns`
and friends are recognised services, so they appear in the process table with
their CPU and memory **regardless of CPU rank** — a kubelet idling at 2%
would never make a top-5-by-CPU list, and its absence would be
indistinguishable from it not running.

### What is deliberately not here
No pod logs, no exec-into-pod, no deployments or replica sets, no cluster
events, no control over Kubernetes objects. The agent is a **node** agent: it
reports what the node is doing. Managing Kubernetes objects is `kubectl`'s
job, and duplicating it is out of scope.

---

## Deployment shapes

**Node agent on a VM or bare metal** — the one-line installer, systemd unit.
Reports the machine's own CPU, memory, disk, NICs and full process table.

**Agent as a DaemonSet** — one pod per node, `hostPID: true` (without it the
agent sees only its own PID namespace and reports a single process),
`hostNetwork: true`, and host mounts of `/proc`, `/sys/fs/cgroup` and
`/var/log/pods`. The node's identity lives on a host path so a pod restart
keeps it instead of re-enrolling as a new machine.

**Caveat when agents run in containers** — `/proc/meminfo`, `/proc/loadavg`
and `/proc/uptime` report the *host*, not the container, and the agent does
not consult its own cgroup limits. That is correct for a node agent. It also
means several containers on one host report identical CPU, memory and load —
which is a property of the test setup, not of the fleet.
