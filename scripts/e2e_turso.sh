#!/bin/sh
# E2E against the Turso (Hrana) store, incl. control-plane restart to
# prove all state lives in the DB (CA material stays on disk by design).
set -eu
cd "$(dirname "$0")/.."
rm -rf /tmp/cp2 /tmp/agent-t /tmp/hrana.db* /tmp/server2.log /tmp/agent-t.log
SERVER_PID=""; AGENT_PID=""; EMU_PID=""
trap 'kill $SERVER_PID $AGENT_PID $EMU_PID 2>/dev/null || true' EXIT

python3 scripts/hrana_emulator.py 9090 /tmp/hrana.db > /tmp/hrana.log 2>&1 &
EMU_PID=$!
sleep 1

start_server() {
  FLEET_ADMIN_TOKEN=testtoken ./bin/fleet-server \
    --data /tmp/cp2 --agent-addr 127.0.0.1:8443 --admin-addr 127.0.0.1:8080 \
    --db-url http://127.0.0.1:9090 >> /tmp/server2.log 2>&1 &
  SERVER_PID=$!
  sleep 1
}
start_server
grep -q 'store: turso' /tmp/server2.log && echo "== turso store selected"

auth='Authorization: Bearer testtoken'
TENANT=$(curl -s -H "$auth" -d '{"name":"pantheonlab"}' 127.0.0.1:8080/admin/tenants | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
TOKEN=$(curl -s -H "$auth" -d "{\"tenant_id\":\"$TENANT\"}" 127.0.0.1:8080/admin/tokens | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
FP=$(sed -n 's/.*ca_sha256=//p' /tmp/server2.log | head -1)

./bin/fleet-agent enroll --server https://127.0.0.1:8443 --data /tmp/agent-t \
  --token "$TOKEN" --fingerprint "$FP" --name turso-node-01
./bin/fleet-agent run --server https://127.0.0.1:8443 --data /tmp/agent-t \
  --heartbeat 2s > /tmp/agent-t.log 2>&1 &
AGENT_PID=$!
sleep 2
MACHINE=$(curl -s -H "$auth" 127.0.0.1:8080/admin/machines | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])')
echo "== enrolled machine $MACHINE"

PAYLOAD=$(printf '#!/bin/sh\necho "turso path ok, cfg=$FLEET_CFG_MODE"\n' | base64 -w0)
curl -s -H "$auth" -d "{\"name\":\"hello\",\"version\":\"2.0.0\",\"payload\":\"$PAYLOAD\"}" 127.0.0.1:8080/admin/modules > /dev/null
curl -s -X PUT -H "$auth" -d '{"modules":[{"name":"hello","version":"2.0.0","config":{"mode":"turso"}}]}' \
  "127.0.0.1:8080/admin/machines/$MACHINE/desired" > /dev/null
sleep 2
STATE=$(curl -s -H "$auth" "127.0.0.1:8080/admin/machines/$MACHINE" | python3 -c 'import sys,json;m=json.load(sys.stdin);print(m["status"][0]["state"], m["status"][0]["detail"].strip())')
echo "== module result: $STATE"

echo "== restarting control plane (agent stays up)..."
kill $SERVER_PID; wait $SERVER_PID 2>/dev/null || true; start_server; sleep 3

AFTER=$(curl -s -H "$auth" "127.0.0.1:8080/admin/machines/$MACHINE" | python3 -c 'import sys,json;m=json.load(sys.stdin);print("machine persisted:",m["name"],"| desired rev:",m["desired"]["revision"],"| status:",m["status"][0]["state"])')
echo "== after restart: $AFTER"
NOW=$(date +%s)
SEEN=$(curl -s -H "$auth" "127.0.0.1:8080/admin/machines/$MACHINE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["last_seen"])')
if [ $((NOW - SEEN)) -le 10 ]; then echo "== agent reconnected and heartbeating (last_seen ${SEEN})"; else echo "== FAIL: stale last_seen"; exit 1; fi
echo "== rows in sqlite:"
python3 -c 'import sqlite3;c=sqlite3.connect("/tmp/hrana.db");[print(" ",t,c.execute(f"select count(*) from {t}").fetchone()[0]) for t in ("tenants","tokens","machines","modules")]'
echo E2E_TURSO_PASS
