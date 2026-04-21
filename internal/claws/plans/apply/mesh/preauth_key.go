package mesh

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

const headscalePreauthUserID = "1"

// getOrResolvePreauthKey returns the Tailscale preauth key for this plan
// run. The value is memoised on the plan cache so the first caller pays
// for the resolution (env var read, SFTP of the key file, or headscale
// CLI mint) and the rest hit the in-memory cache.
//
// Resolution is an on-demand helper rather than a dedicated plan step:
// a step whose only job is to populate an in-memory cache would always
// render "will execute" in the plan tree even on a fully-converged
// system, which is noise. Supported sources (from gw.Networking):
// "env", "file", "auto" (env then file).
//
// gwMT must describe the headscale gateway host (IsGatewayHost==true,
// Gateway!=nil). Callers on non-gateway targets synthesise one via
// InstallTailscaleStep.lookupGatewayMeshTarget.
func getOrResolvePreauthKey(ctx context.Context, dial SSHDialFunc, gwMT *MeshTarget) (string, error) {
	if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey); ok {
		if s, _ := v.(string); s != "" {
			return s, nil
		}
	}
	key, err := resolvePreauthKey(ctx, dial, gwMT)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("resolve preauth key: empty after resolution")
	}
	scaffold.PlanCacheSet(ctx, CacheKeyPreauthKey, key)
	return key, nil
}

func resolvePreauthKey(ctx context.Context, dial SSHDialFunc, mt *MeshTarget) (string, error) {
	gw := mt.Gateway
	if gw == nil || gw.Networking == nil {
		return "", fmt.Errorf("resolve preauth key: gateway has no networking block")
	}

	src := strings.ToLower(strings.TrimSpace(gw.Networking.PreauthKeySource))
	if src == "" {
		src = "auto"
	}
	envName := strings.TrimSpace(gw.Networking.PreauthKeyEnv)
	filePath := strings.TrimSpace(gw.Networking.PreauthKeyFile)

	tryEnv := func() string {
		if envName == "" {
			return ""
		}
		return strings.TrimSpace(os.Getenv(envName))
	}

	tryFile := func() (string, error) {
		if filePath == "" {
			return "", fmt.Errorf("preauth_key_file is empty")
		}
		m := mt.Machine
		client, key, err := common.BorrowSSHWithRetry(ctx, dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
		if err != nil {
			return "", fmt.Errorf("dial gateway for preauth key: %w", err)
		}
		defer common.ReturnSSH(ctx, key, client)

		data, err := sshfile.ReadFile(client, filePath)
		if err == nil {
			if k := strings.TrimSpace(string(data)); k != "" {
				return k, nil
			}
		}
		return createPreauthKeyOnGateway(client, filePath)
	}

	switch src {
	case "env":
		v := tryEnv()
		if v == "" {
			return "", fmt.Errorf("resolve preauth key: env var %s is not set", envName)
		}
		return v, nil
	case "file":
		return tryFile()
	case "auto":
		if v := tryEnv(); v != "" {
			return v, nil
		}
		return tryFile()
	default:
		return "", fmt.Errorf("resolve preauth key: unknown preauth_key_source %q", src)
	}
}

// createPreauthKeyOnGateway uses the headscale CLI on the gateway to create
// a reusable preauth key, writes it to filePath, and returns it.
func createPreauthKeyOnGateway(client *xssh.Client, filePath string) (string, error) {
	script := fmt.Sprintf(`set -e
path=%q
sudo mkdir -p "$(dirname "$path")"
HS=/usr/local/bin/headscale
if ! sudo test -x "$HS"; then
  echo "headscale not found" >&2
  exit 1
fi
OUT=$(sudo "$HS" preauthkeys create --user default --expiration 720h --reusable 2>&1) || true
KEY=$(echo "$OUT" | grep -Eo '[ht]skey-auth-[A-Za-z0-9_=-]+' | head -1)
if [ -z "$KEY" ]; then
  OUT=$(sudo "$HS" preauthkeys create --user %s --expiration 720h --reusable 2>&1) || true
  KEY=$(echo "$OUT" | grep -Eo '[ht]skey-auth-[A-Za-z0-9_=-]+' | head -1)
fi
if [ -z "$KEY" ]; then
  OUT2=$(sudo "$HS" preauthkeys create -u %s -e 720h --reusable 2>&1) || true
  KEY=$(echo "$OUT2" | grep -Eo '[ht]skey-auth-[A-Za-z0-9_=-]+' | head -1)
fi
if [ -z "$KEY" ]; then
  echo "could not create headscale preauth key. Output: $OUT" >&2
  exit 1
fi
printf '%%s\n' "$KEY" | sudo tee "$path" >/dev/null
sudo chmod 600 "$path"
echo "$KEY"
`, filePath, headscalePreauthUserID, headscalePreauthUserID)

	out, err := bash.RunOutput(client, script)
	if err != nil {
		return "", fmt.Errorf("create preauth key on gateway: %w\n%s", err, out)
	}
	key := strings.TrimSpace(out)
	if key == "" {
		return "", fmt.Errorf("preauth key creation returned empty output")
	}
	return key, nil
}
