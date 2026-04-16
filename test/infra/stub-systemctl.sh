#!/bin/sh
# Stub systemctl for Docker integration tests.
# Handles openclaw-gateway and openclaw-node start/stop; everything else is a no-op.
echo "[stub-systemctl] $*" >&2

SUBCMD=""
UNIT=""
NOW=false
for arg in "$@"; do
  case "$arg" in
    --user|--no-pager|--quiet|-q) ;;
    --now) NOW=true ;;
    is-active|is-enabled|status|start|stop|restart|reload|enable|disable|daemon-reload|mask|unmask) SUBCMD="$arg" ;;
    -*) ;;
    *) [ -z "$UNIT" ] && UNIT="$arg" ;;
  esac
done

# Load Environment= directives from both the unit file and the drop-in.
# Strips optional surrounding quotes from values.
load_unit_env() {
  local svc="$1"
  for d in "$HOME/.config/systemd/user" "/etc/systemd/system"; do
    local unit="$d/${svc}.service"
    if [ -f "$unit" ]; then
      eval "$(grep '^Environment=' "$unit" | sed 's/^Environment=/export /' | sed 's/export "\(.*\)"/export \1/')"
    fi
    local dropin="$d/${svc}.service.d/env.conf"
    if [ -f "$dropin" ]; then
      eval "$(grep '^Environment=' "$dropin" | sed 's/^Environment=/export /' | sed 's/export "\(.*\)"/export \1/')"
    fi
  done
}

extract_exec_start() {
  local svc="$1"
  for d in "$HOME/.config/systemd/user" "/etc/systemd/system"; do
    local f="$d/${svc}.service"
    if [ -f "$f" ]; then
      grep '^ExecStart=' "$f" | sed 's/^ExecStart=//' | head -1
      return
    fi
  done
}

do_stop() {
  case "$1" in
    openclaw-gateway*)
      pkill -f "openclaw/dist/index.js gateway" 2>/dev/null || true
      pkill -f "openclaw gateway" 2>/dev/null || true
      sleep 1
    ;;
    openclaw-node*)
      pkill -f "openclaw/dist/index.js node" 2>/dev/null || true
      pkill -f "openclaw node" 2>/dev/null || true
      sleep 1
    ;;
    headscale*)
      pkill -f "headscale serve" 2>/dev/null || true
      sleep 1
    ;;
    tailscaled*)
      pkill -f "tailscaled" 2>/dev/null || true
      sleep 1
    ;;
  esac
}

do_start() {
  case "$UNIT" in
    openclaw-gateway*)
      do_stop "$UNIT"
      load_unit_env "openclaw-gateway"
      EXEC=$(extract_exec_start "openclaw-gateway")
      if [ -n "$EXEC" ]; then
        nohup sh -c "$EXEC" >> /tmp/openclaw-gateway.log 2>&1 &
      else
        OC=$(command -v openclaw 2>/dev/null)
        [ -n "$OC" ] && nohup "$OC" gateway --allow-unconfigured >> /tmp/openclaw-gateway.log 2>&1 &
      fi
    ;;
    openclaw-node*)
      do_stop "$UNIT"
      load_unit_env "openclaw-node"
      EXEC=$(extract_exec_start "openclaw-node")
      if [ -n "$EXEC" ]; then
        nohup sh -c "$EXEC" >> /tmp/openclaw-node.log 2>&1 &
      else
        OC=$(command -v openclaw 2>/dev/null)
        [ -n "$OC" ] && nohup "$OC" node run >> /tmp/openclaw-node.log 2>&1 &
      fi
    ;;
    headscale*)
      do_stop "$UNIT"
      load_unit_env "headscale"
      EXEC=$(extract_exec_start "headscale")
      if [ -n "$EXEC" ]; then
        nohup sh -c "$EXEC" >> /tmp/headscale.log 2>&1 &
      else
        nohup /usr/local/bin/headscale serve --config /etc/headscale/config.yaml >> /tmp/headscale.log 2>&1 &
      fi
    ;;
    tailscaled*)
      do_stop "$UNIT"
      nohup tailscaled --state=/var/lib/tailscale/tailscaled.state >> /tmp/tailscaled.log 2>&1 &
      sleep 1
    ;;
  esac
}

case "$SUBCMD" in
  is-active)  exit 0 ;;
  is-enabled) exit 0 ;;
  status) printf "● %s - stub\n   Active: active (running)\n" "$UNIT"; exit 0 ;;
  start|restart) do_start; exit 0 ;;
  enable) $NOW && do_start; exit 0 ;;
  stop) do_stop "$UNIT"; exit 0 ;;
  *) exit 0 ;;
esac
