#!/usr/bin/env bash
# Adds the active claws SSH public key to a Multipass VM user's authorized_keys.
set -euo pipefail

read -rp "Multipass VM name: " vm_name
read -rp "SSH user on the VM [default: ubuntu]: " vm_user
vm_user="${vm_user:-ubuntu}"

list_output=$(go run ./cmd/cli auth list)

priv_key_path=$(echo "$list_output" | awk 'NR>1 && $1=="*" { print $(NF-1) }' | tr -d '\r')
pub_key_path=$(echo "$list_output"  | awk 'NR>1 && $1=="*" { print $NF }'      | tr -d '\r')

# Expand leading ~ since tilde expansion doesn't happen inside quoted variables.
priv_key_path="${priv_key_path/#\~/$HOME}"
pub_key_path="${pub_key_path/#\~/$HOME}"

if [[ -z "$pub_key_path" ]]; then
    echo "Error: no active claws SSH identity found. Run: claws auth generate <name> && claws auth use <name>" >&2
    exit 1
fi

if [[ ! -f "$pub_key_path" ]]; then
    echo "Error: public key not found at: $pub_key_path" >&2
    exit 1
fi
if [[ ! -f "$priv_key_path" ]]; then
    echo "Error: private key not found at: $priv_key_path" >&2
    exit 1
fi

pub_key=$(cat "$pub_key_path")
home_dir=$([ "$vm_user" = "root" ] && echo "/root" || echo "/home/$vm_user")

echo "Adding key from $pub_key_path to ${vm_user}@${vm_name} ..."

multipass exec "$vm_name" -- bash -c "
    mkdir -p ${home_dir}/.ssh
    chmod 700 ${home_dir}/.ssh
    grep -qxF '${pub_key}' ${home_dir}/.ssh/authorized_keys 2>/dev/null || echo '${pub_key}' >> ${home_dir}/.ssh/authorized_keys
    chmod 600 ${home_dir}/.ssh/authorized_keys
"

vm_ip=$(multipass info "$vm_name" --format json | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['info']['${vm_name}']['ipv4'][0])" 2>/dev/null || true)

if [[ -z "$vm_ip" ]]; then
    echo "Warning: could not resolve IP for $vm_name — skipping SSH test." >&2
else
    echo "Testing SSH connection to ${vm_user}@${vm_ip} using private key ..."
    result=$(ssh -i "$priv_key_path" \
        -o StrictHostKeyChecking=no \
        -o BatchMode=yes \
        -o ConnectTimeout=10 \
        "${vm_user}@${vm_ip}" whoami 2>&1)
    if [[ "$result" == "$vm_user" ]]; then
        echo "SSH test passed — logged in as '$result'."
    else
        echo "Error: SSH test failed (got: $result)" >&2
        exit 1
    fi
fi

echo "Done. claws can now SSH into ${vm_name} as ${vm_user}."
