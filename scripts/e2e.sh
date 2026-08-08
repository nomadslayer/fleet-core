#!/bin/sh
# End-to-end smoke test: server up -> tenant -> token -> enroll -> heartbeat
# -> upload signed module -> set desired state -> module applied & reported.
set -eu
cd "$(dirname "$0")/.."
rm -rf /tmp/cp /tmp/agent /tmp/server.log /tmp/agent.log

FLEET_ADMIN_TOKEN=testtoken ./bin/fleet-server \
  --data /tmp/cp --agent-addr 127.0.0.1:8443 --admin-addr 127.0.0.1:8080 \
  > /tmp/server.log 2>&1 &
SERVER_PID=$!
AGENT_PID=""
trap 'kill $SERVER_PID $AGENT_PID 2>/dev/null || true' EXIT
sleep 1

auth='Authorization: Bearer testtoken'
TENANT=$(curl -s -H "$auth" -d '{"name":"pantheonlab"}' 127.0.0.1:8080/admin/tenants | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
TOKEN=$(curl -s -H "$auth" -d "{\"tenant_id\":\"$TENANT\"}" 127.0.0.1:8080/admin/tokens | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
FP=$(sed -n 's/.*ca_sha256=//p' /tmp/server.log | head -1)
echo "== tenant=$TENANT fp=$FP"

./bin/fleet-agent enroll --server https://127.0.0.1:8443 --data /tmp/agent \
  --token "$TOKEN" --fingerprint "$FP" --name test-node-01
./bin/fleet-agent run --server https://127.0.0.1:8443 --data /tmp/agent \
  --heartbeat 3s > /tmp/agent.log 2>&1 &
AGENT_PID=$!
sleep 2

echo "== machine after heartbeat:"
curl -s -H "$auth" 127.0.0.1:8080/admin/machines | python3 -m json.tool

MACHINE=$(curl -s -H "$auth" 127.0.0.1:8080/admin/machines | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])')

# Upload a hello module (shell script payload), signed server-side.
PAYLOAD=$(printf '#!/bin/sh\necho "hello from $FLEET_MODULE_NAME on $FLEET_MACHINE_ID, greeting=$FLEET_CFG_GREETING"\n' | base64 -w0)
curl -s -H "$auth" -d "{\"name\":\"hello\",\"version\":\"1.0.0\",\"payload\":\"$PAYLOAD\"}" \
  127.0.0.1:8080/admin/modules | python3 -m json.tool

# Desire it on the machine.
curl -s -X PUT -H "$auth" \
  -d '{"modules":[{"name":"hello","version":"1.0.0","config":{"greeting":"kev"}}]}' \
  "127.0.0.1:8080/admin/machines/$MACHINE/desired" | python3 -m json.tool
sleep 2

echo "== machine status after reconcile:"
curl -s -H "$auth" "127.0.0.1:8080/admin/machines/$MACHINE" | python3 -m json.tool
echo "== negative test: reused enrollment token must be rejected:"
./bin/fleet-agent enroll --server https://127.0.0.1:8443 --data /tmp/agent2 \
  --token "$TOKEN" --fingerprint "$FP" 2>&1 | tail -1 || true
echo "== agent log:"
cat /tmp/agent.log
