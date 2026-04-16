#!/bin/sh
# Stub tailscaled daemon for Docker integration tests.
# Creates both unix socket paths that install_tailscale.go waits for,
# then sleeps to stay alive as a background daemon.
mkdir -p /run/tailscale /var/run/tailscale
python3 -c "import socket,os,time;p='/run/tailscale/tailscaled.sock';s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);s.bind(p);os.chmod(p,0o660);s.listen(5);time.sleep(86400)" &
python3 -c "import socket,os,time;p='/var/run/tailscale/tailscaled.sock';s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);s.bind(p);os.chmod(p,0o660);s.listen(5);time.sleep(86400)" &
wait
