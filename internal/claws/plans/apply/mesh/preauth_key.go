package mesh

import (
	"context"
	"errors"
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

// Check implements scaffold.Step — asks the read-only resolver whether a
// preauth key is already available (env var set, or on-disk on the gateway).
// Pure read: does NOT invoke the headscale CLI to mint a fresh key — that
// is a real side effect on the remote system and belongs in Execute.
//
// A cold cache with a pre-existing key file converges to the same verdict
// as a hot cache, per .cursor/rules/plan-checks-are-pure.mdc (cold == hot).
func (s *ResolvePreauthKeyStep) Check(ctx context.Context, t scaffold.Target) (bool, error) {
	mt, ok := t.Payload.(*MeshTarget)
	if !ok || mt == nil || mt.Gateway == nil {
		return false, nil
	}
	key, err := getOrResolvePreauthKeyReadOnly(ctx, s.dial, mt)
	if err != nil {
		return false, err
	}
	return key != "", nil
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
		client, key, err := common.BorrowSSHWithRetry(ctx, s.dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
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

// getOrResolvePreauthKeyReadOnly is the Check-safe variant of
// getOrResolvePreauthKey: it consults the plan cache, then the environment,
// then the file on disk, but NEVER invokes the headscale CLI to mint a
// fresh key. Minting is a real side effect on the remote gateway and is
// reserved for Execute.
//
// Returns ("", nil) when no pre-existing key is reachable (cold file,
// unset env) — that's the legitimate "not yet provisioned" verdict, not
// an error. Dial / SFTP failures are propagated as errors so the probe
// UI can render "unknown" instead of lying as "will execute".
func getOrResolvePreauthKeyReadOnly(ctx context.Context, dial SSHDialFunc, mt *MeshTarget) (string, error) {
	if v, ok := scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey); ok {
		if s, _ := v.(string); s != "" {
			return s, nil
		}
	}
	gw := mt.Gateway
	if gw == nil || gw.Networking == nil {
		return "", nil
	}
	src := strings.ToLower(strings.TrimSpace(gw.Networking.PreauthKeySource))
	if src == "" {
		src = "auto"
	}
	envName := strings.TrimSpace(gw.Networking.PreauthKeyEnv)
	filePath := strings.TrimSpace(gw.Networking.PreauthKeyFile)

	if envName != "" {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			scaffold.PlanCacheSet(ctx, CacheKeyPreauthKey, v)
			return v, nil
		}
	}
	if src == "env" {
		return "", nil
	}
	if filePath == "" {
		return "", nil
	}
	m := mt.Machine
	client, key, err := common.BorrowSSH(ctx, dial, common.ResolveMachineHost(ctx, m), common.MachineSSHPort(m), common.MachineAgentUser(m))
	if err != nil {
		return "", fmt.Errorf("dial gateway for preauth key: %w", err)
	}
	defer common.ReturnSSH(ctx, key, client)

	data, err := sshfile.ReadFile(client, filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read preauth key file: %w", err)
	}
	k := strings.TrimSpace(string(data))
	if k != "" {
		scaffold.PlanCacheSet(ctx, CacheKeyPreauthKey, k)
	}
	return k, nil
}

// getOrResolvePreauthKey returns the tailscale preauth key for this plan run,
// preferring the plan-cache entry seeded by resolve-preauth-key. On a cold
// start (`claws apply --only mesh-join` without the mesh-gateway phase
// running in this process) the cache is empty; we then re-derive the key
// by re-running the manifest's preauth_key_source logic against the live
// gateway VM (env → file → auto; file reads from disk, otherwise create a
// fresh key via the headscale CLI). This makes mesh-join idempotent across
// separate CLI invocations without requiring the gateway phase to run in
// the same process.
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
	step := &ResolvePreauthKeyStep{dial: dial}
	if err := step.Execute(ctx, scaffold.Target{ID: gwMT.Machine.Name, Payload: gwMT}); err != nil {
		return "", err
	}
	v, ok := scaffold.PlanCacheGet(ctx, CacheKeyPreauthKey)
	if !ok {
		return "", fmt.Errorf("preauth key not in cache after resolve")
	}
	s, _ := v.(string)
	if s == "" {
		return "", fmt.Errorf("preauth key resolved to empty string")
	}
	return s, nil
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
