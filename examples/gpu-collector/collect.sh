#!/bin/sh
# fleetcore GPU collector module — deploy to the gpu-fleet group.
#
# Polls nvidia-smi and pushes per-GPU utilization to the local agent's
# metrics endpoint, which forwards it to the control plane's live
# registry. Keeps NVML/CUDA entirely out of the agent: this module only
# runs where GPUs exist, and only it needs the driver.
#
# The agent exposes a localhost push socket for modules at
# $FLEET_LOCAL_PUSH (set by the agent when running a collector module).
# Config (FLEET_CFG_*): interval (seconds, default 2).
#
# This is a long-running module: it loops until killed. The agent
# supervises it as a child; a future supervisor module adds restart.
set -eu

INTERVAL="${FLEET_CFG_INTERVAL:-2}"
PUSH="${FLEET_LOCAL_PUSH:?agent did not provide FLEET_LOCAL_PUSH}"

command -v nvidia-smi >/dev/null 2>&1 || { echo "nvidia-smi not found"; exit 1; }

echo "[gpu] collecting every ${INTERVAL}s -> ${PUSH}"
while true; do
  # index, name, util%, mem used MiB, mem total MiB, temp C, power W
  JSON=$(nvidia-smi \
    --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw \
    --format=csv,noheader,nounits 2>/dev/null | awk -F', *' '
    BEGIN { printf "{\"at_unix\":%d,\"gpus\":[", systime() }
    {
      if (NR>1) printf ","
      printf "{\"index\":%d,\"name\":\"%s\",\"util_percent\":%s,\"mem_used\":%d,\"mem_total\":%d,\"temp_c\":%s,\"power_w\":%s}", \
        $1, $2, $3, $4*1048576, $5*1048576, $6, $7
    }
    END { printf "]}" }')

  # push to the agent's local module-metrics endpoint
  printf '%s' "$JSON" | "$PUSH" 2>/dev/null || echo "[gpu] push failed"
  sleep "$INTERVAL"
done
