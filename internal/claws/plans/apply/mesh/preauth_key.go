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

// ResolvePreauthKeyStep resolves the Tailscale preauth key and stores it in
// the plan cache. Supports sources: env, file, auto.
type ResolvePreauthKeyStep struct {
	dial SSHDialFunc
}

func NewResolvePreauthKeyStep(opts Options) *ResolvePreauthKeyStep {
	return &ResolvePreauthKeyStep{dial: opts.SSHDial}
}

func (*ResolvePreauthKeyStep) Name() string { return "resolve-preauth-key" }

func (*ResolvePreauthKeyStep) Applicable(_ context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	return ok && mt.IsGatewayHost && mt.Gateway != nil, nil
}

func (*ResolvePreauthKeyStep) Check(ctx context.Context, _ scaffold.Target) (bool, error) {
	v, ok := scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey)
	if !ok {
		return false, nil
	}
	s, _ := v.(string)
	return s != "", nil
}

func (s *ResolvePreauthKeyStep) Execute(ctx context.Context, t scaffold.Target) error {
	mt := t.Payload.(*MeshTarget)
	gw := mt.Gateway
	if gw.Networking == nil {
		return fmt.Errorf("resolve-preauth-key: gateway %q has no networking block", gw.Name)
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
		client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineSSHUser(m))
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

	var preauthKey string
	var err error

	switch src {
	case "env":
		v := tryEnv()
		if v == "" {
			return fmt.Errorf("resolve-preauth-key: env var %s is not set", envName)
		}
		preauthKey = v
	case "file":
		preauthKey, err = tryFile()
	case "auto":
		if v := tryEnv(); v != "" {
			preauthKey = v
		} else {
			preauthKey, err = tryFile()
		}
	default:
		return fmt.Errorf("resolve-preauth-key: unknown preauth_key_source %q", src)
	}

	if err != nil {
		return fmt.Errorf("resolve-preauth-key: %w", err)
	}
	if preauthKey == "" {
		return fmt.Errorf("resolve-preauth-key: preauth key is empty after resolution")
	}

	scaffold.PlanCacheSet(ctx, CacheKeyPreauthKey, preauthKey)
	return nil
}

func (*ResolvePreauthKeyStep) Verify(ctx context.Context, _ scaffold.Target) error {
	v, ok := scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey)
	if !ok {
		return fmt.Errorf("resolve-preauth-key verify: key not in cache")
	}
	if v.(string) == "" {
		return fmt.Errorf("resolve-preauth-key verify: key is empty")
	}
	return nil
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
