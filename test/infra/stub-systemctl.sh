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

# Load environment from the systemd env drop-in written by configure-gateway.
load_dropin_env() {
  local svc="$1"
  for d in "$HOME/.config/systemd/user" "/etc/systemd/system"; do
    local f="$d/${svc}.service.d/env.conf"
    if [ -f "$f" ]; then
      eval "$(grep '^Environment=' "$f" | sed 's/^Environment=/export /')"
      return
    fi
  done
}

do_start() {
  case "$UNIT" in
    openclaw-gateway*)
      pkill -f "openclaw gateway" 2>/dev/null || true; sleep 1
      OC=$(command -v openclaw 2>/dev/null)
      if [ -n "$OC" ]; then
        load_dropin_env "openclaw-gateway"
        nohup "$OC" gateway --allow-unconfigured >> /tmp/openclaw-gateway.log 2>&1 &
      fi
    ;;
    openclaw-node*)
      pkill -f "openclaw node" 2>/dev/null || true; sleep 1
      OC=$(command -v openclaw 2>/dev/null)
      if [ -n "$OC" ]; then
        load_dropin_env "openclaw-node"
        nohup "$OC" node run >> /tmp/openclaw-node.log 2>&1 &
      fi
    ;;
  esac
}

case "$SUBCMD" in
  is-active)  exit 0 ;;
  is-enabled) exit 0 ;;
  status) printf "● %s - stub\n   Active: active (running)\n" "$UNIT"; exit 0 ;;
  start|restart) do_start; exit 0 ;;
  enable) $NOW && do_start; exit 0 ;;
  stop)
    pkill -f "openclaw gateway" 2>/dev/null || true
    pkill -f "openclaw node" 2>/dev/null || true
    exit 0 ;;
  *) exit 0 ;;
esac
