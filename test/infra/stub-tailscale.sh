#!/bin/sh
# Stub tailscale CLI for Docker integration tests.
# - up: writes a deterministic fake IPv4 to /var/lib/tailscale/ip4.txt
# - ip -4: reads and prints the fake IP (empty string if not yet joined)
# - all other subcommands: succeed silently
SUBCMD="$1"; shift
case "$SUBCMD" in
  up)
    mkdir -p /var/lib/tailscale
    printf '100.64.0.1\n' > /var/lib/tailscale/ip4.txt
    ;;
  ip)
    if [ "$1" = "-4" ]; then
      cat /var/lib/tailscale/ip4.txt 2>/dev/null || true
    fi
    ;;
  *)
    exit 0
    ;;
esac
