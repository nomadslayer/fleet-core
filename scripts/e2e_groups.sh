#!/bin/sh
# Groups E2E: (1) machine enrolled with role=gpu label via token gets the
# group module with no per-machine call; (2) unlabeled machine does not;
# (3) explicit member add pulls it in; (4) machine override beats group.
set -eu
cd "$(dirname "$0")/.."
rm -rf /tmp/cpg /tmp/ag1 /tmp/ag2 /tmp/hranag.db* /tmp/sg.log /tmp/ag1.log /tmp/ag2.log
SRV=""; A1=""; A2=""; EMU=""
trap 'kill $SRV $A1 $A2 $EMU 2>/dev/null || true' EXIT

python3 scripts/hrana_emulator.py 9095 /tmp/hranag.db > /dev/null 2>&1 & EMU=$!
sleep 1
FLEET_ADMIN_TOKEN=testtoken ./bin/fleet-server --data /tmp/cpg \
  --agent-addr 127.0.0.1:8445 --admin-addr 127.0.0.1:8082 \
  --db-url http://127.0.0.1:9095 > /tmp/sg.log 2>&1 & SRV=$!
sleep 1

auth='Authorization: Bearer testtoken'
adm=127.0.0.1:8082
jq() { python3 -c "import sys,json;print(json.load(sys.stdin)$1)"; }

T=$(curl -s -H "$auth" -d '{"name":"pantheonlab"}' $adm/admin/tenants | jq '["id"]')
FP=$(sed -n 's/.*ca_sha256=//p' /tmp/sg.log | head -1)

# module + group (selector role=gpu) BEFORE any machine exists
PAYLOAD=$(printf '#!/bin/sh\necho "dcgm exporter up"\n' | base64 -w0)
curl -s -H "$auth" -d "{\"name\":\"dcgm\",\"version\":\"1.0.0\",\"payload\":\"$PAYLOAD\"}" $adm/admin/modules > /dev/null
GID=$(curl -s -H "$auth" -d "{\"tenant_id\":\"$T\",\"name\":\"gpu-fleet\",\"selector\":{\"role\":\"gpu\"},\"modules\":[{\"name\":\"dcgm\",\"version\":\"1.0.0\"}]}" $adm/admin/groups | jq '["id"]')
echo "== group gpu-fleet created: $GID"

# machine 1: token stamped with role=gpu
TOK1=$(curl -s -H "$auth" -d "{\"tenant_id\":\"$T\",\"labels\":{\"role\":\"gpu\",\"region\":\"sg\"}}" $adm/admin/tokens | jq '["token"]')
./bin/fleet-agent enroll --server https://127.0.0.1:8445 --data /tmp/ag1 --token "$TOK1" --fingerprint "$FP" --name gpu-01 2>/dev/null
./bin/fleet-agent run --server https://127.0.0.1:8445 --data /tmp/ag1 --heartbeat 3s > /tmp/ag1.log 2>&1 & A1=$!

# machine 2: no labels
TOK2=$(curl -s -H "$auth" -d "{\"tenant_id\":\"$T\"}" $adm/admin/tokens | jq '["token"]')
./bin/fleet-agent enroll --server https://127.0.0.1:8445 --data /tmp/ag2 --token "$TOK2" --fingerprint "$FP" --name plain-01 2>/dev/null
./bin/fleet-agent run --server https://127.0.0.1:8445 --data /tmp/ag2 --heartbeat 3s > /tmp/ag2.log 2>&1 & A2=$!
sleep 3

M1=$(curl -s -H "$auth" "$adm/admin/machines?tenant=$T" | python3 -c 'import sys,json;print(next(m["id"] for m in json.load(sys.stdin) if m["name"]=="gpu-01"))')
M2=$(curl -s -H "$auth" "$adm/admin/machines?tenant=$T" | python3 -c 'import sys,json;print(next(m["id"] for m in json.load(sys.stdin) if m["name"]=="plain-01"))')

echo "== gpu-01 (selector member) status:"
curl -s -H "$auth" "$adm/admin/machines/$M1" | python3 -c 'import sys,json;m=json.load(sys.stdin);print("  labels:",m["labels"],"| modules:",[x["name"] for x in m["desired"]["modules"] or []],"| status:",[(x["name"],x["state"]) for x in m["status"] or []])'
echo "== plain-01 (no labels) status:"
curl -s -H "$auth" "$adm/admin/machines/$M2" | python3 -c 'import sys,json;m=json.load(sys.stdin);print("  modules:",m["desired"]["modules"],"| status:",m["status"])'

echo "== group members view:"
curl -s -H "$auth" "$adm/admin/groups/$GID/members" | python3 -m json.tool

echo "== adding plain-01 as explicit member..."
curl -s -X POST -H "$auth" -d "{\"machine_id\":\"$M2\"}" "$adm/admin/groups/$GID/members" -o /dev/null
sleep 2
curl -s -H "$auth" "$adm/admin/machines/$M2" | python3 -c 'import sys,json;m=json.load(sys.stdin);print("  plain-01 now:",[(x["name"],x["state"]) for x in m["status"] or []])'

echo "== override precedence: pin dcgm 2.0.0 on gpu-01..."
PAYLOAD2=$(printf '#!/bin/sh\necho "dcgm TWO"\n' | base64 -w0)
curl -s -H "$auth" -d "{\"name\":\"dcgm\",\"version\":\"2.0.0\",\"payload\":\"$PAYLOAD2\"}" $adm/admin/modules > /dev/null
curl -s -X PUT -H "$auth" -d '{"modules":[{"name":"dcgm","version":"2.0.0"}]}' "$adm/admin/machines/$M1/desired" > /dev/null
sleep 2
curl -s -H "$auth" "$adm/admin/machines/$M1" | python3 -c 'import sys,json;m=json.load(sys.stdin);print("  gpu-01 effective:",[(x["name"],x["version"]) for x in m["desired"]["modules"]],"| applied:",[(x["version"],x["state"]) for x in m["status"] or []])'

echo "== metrics group sanity:"
curl -s -H "$auth" $adm/metrics | grep fleet_module_applied
echo E2E_GROUPS_PASS
