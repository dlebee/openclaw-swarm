#!/bin/sh
# Stub systemctl for Docker integration tests.
# Handles openclaw-gateway and openclaw-node start/stop; everything else is a no-op.
echo "[stub-systemctl] $*" >&2

SUBCMD=""
UNIT=""
for arg in "$@"; do
  case "$arg" in
    --user|--no-pager|--quiet|-q) ;;
    is-active|is-enabled|status|start|stop|restart|reload|enable|disable|daemon-reload|mask|unmask) SUBCMD="$arg" ;;
    -*) ;;
    *) [ -z "$UNIT" ] && UNIT="$arg" ;;
  esac
done

case "$SUBCMD" in
  is-active)  echo "active"; exit 0 ;;
  is-enabled) echo "enabled"; exit 0 ;;
  status) printf "● %s - stub\n   Active: active (running)\n" "$UNIT"; exit 0 ;;
  start|restart)
    case "$UNIT" in
      openclaw-gateway*)
        pkill -f "openclaw gateway" 2>/dev/null || true; sleep 1
        OC=$(command -v openclaw 2>/dev/null)
        [ -n "$OC" ] && nohup "$OC" gateway --allow-unconfigured >> /tmp/openclaw-gateway.log 2>&1 &
      ;;
      openclaw-node*)
        pkill -f "openclaw node" 2>/dev/null || true; sleep 1
        OC=$(command -v openclaw 2>/dev/null)
        [ -n "$OC" ] && nohup env OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 "$OC" node run >> /tmp/openclaw-node.log 2>&1 &
      ;;
    esac; exit 0 ;;
  stop)
    pkill -f "openclaw gateway" 2>/dev/null || true
    pkill -f "openclaw node" 2>/dev/null || true
    exit 0 ;;
  *) exit 0 ;;
esac
