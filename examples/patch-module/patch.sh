#!/bin/sh
# fleetcore patch module — the "acting" half of package/patch management.
# Detection lives in the agent (reported via inventory); this module,
# pushed as a signed payload to a group, performs the upgrade.
#
# Config (FLEET_CFG_* from the module spec):
#   mode      = security | all        (default: security)
#   reboot    = auto | never          (default: never) — reboot if required
#   refresh   = yes | no              (default: yes)  — update package index first
#
# Idempotent: re-running when nothing is pending is a no-op. Exit 0 =
# applied (the agent records this); any failure exits non-zero and the
# control plane shows the module as failed with captured output.
set -eu

MODE="${FLEET_CFG_MODE:-security}"
REBOOT="${FLEET_CFG_REBOOT:-never}"
REFRESH="${FLEET_CFG_REFRESH:-yes}"

log() { echo "[patch] $*"; }

if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  [ "$REFRESH" = yes ] && { log "apt-get update"; apt-get update -qq; }
  if [ "$MODE" = all ]; then
    log "upgrading all packages"
    apt-get -y -qq upgrade
  else
    log "applying security updates only"
    # Restrict to the -security suite present on Debian/Ubuntu.
    if grep -rhoE '^deb .*-security' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null | head -1 >/dev/null; then
      CODENAME=$(. /etc/os-release; echo "${VERSION_CODENAME:-stable}")
      apt-get -y -qq -o Dir::Etc::SourceList=/dev/null \
        -o Dir::Etc::SourceParts="/etc/apt/sources.list.d" \
        upgrade -t "${CODENAME}-security" || apt-get -y -qq upgrade
    else
      apt-get -y -qq upgrade
    fi
  fi

elif command -v dnf >/dev/null 2>&1; then
  if [ "$MODE" = all ]; then
    log "dnf upgrade (all)"
    dnf -y -q upgrade
  else
    log "dnf upgrade (security)"
    dnf -y -q upgrade --security || dnf -y -q upgrade
  fi

else
  log "no supported package manager (apt/dnf) found"
  exit 1
fi

if [ -f /var/run/reboot-required ]; then
  if [ "$REBOOT" = auto ]; then
    log "reboot required — rebooting in 1 minute"
    (sleep 60; systemctl reboot) &
  else
    log "reboot required (not rebooting; reboot=never)"
  fi
fi
log "done (mode=$MODE)"
