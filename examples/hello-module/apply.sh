#!/bin/sh
# Example fleetcore module: one-shot apply. Idempotency is the module's job.
set -eu
echo "applying ${FLEET_MODULE_NAME}@${FLEET_MODULE_VERSION} on ${FLEET_MACHINE_ID}"
echo "config greeting=${FLEET_CFG_GREETING:-none}"
