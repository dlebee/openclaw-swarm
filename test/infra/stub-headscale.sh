#!/bin/sh
# Stub headscale binary for mesh integration tests.
# Simulates the happy path so the install-headscale step works without a real
# Headscale installation.
SUBCMD="$1"; shift
case "$SUBCMD" in
  version)
    echo "0.28.0"
    ;;
  users)
    echo "User created"
    ;;
  preauthkeys)
    # Support --user / -u / --expiration / -e / --reusable flags; output a fake key.
    echo "tskey-auth-testmeshkey1234567890AB"
    ;;
  serve)
    # Create the unix socket that install-headscale waits for, then sleep.
    mkdir -p /var/run/headscale
    rm -f /var/run/headscale/headscale.sock
    python3 -c "import socket,os,time;p='/var/run/headscale/headscale.sock';s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);s.bind(p);os.chmod(p,0o660);s.listen(5);time.sleep(86400)" &
    wait
    ;;
  *)
    exit 0
    ;;
esac
