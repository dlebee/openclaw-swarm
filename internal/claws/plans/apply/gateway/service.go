package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	xssh "golang.org/x/crypto/ssh"
)

// Re-exports for callers still referencing the legacy type names.
// The canonical definitions now live in package common so both the
// CLI and snapshot ConfigReader implementations can produce them.
type (
	DeviceList    = common.DeviceList
	PendingDevice = common.PendingDevice
	PairedDevice  = common.PairedDevice
)

const (
	gatewayUnit     = "openclaw-gateway"
	gatewayPort     = 18789
	configFile = ".openclaw/openclaw.json"
	healthRetries   = 60
	healthDelay     = 2 * time.Second
	waitRetries     = 10
	waitDelay       = 2 * time.Second
)

// NeedsLANBind reports whether this gateway should bind to 0.0.0.0 (lan)
// instead of loopback. True for docker, headscale, and VPC networking modes
// where nodes communicate over a real network.
func NeedsLANBind(gw manifestdata.Gateway) bool {
	if gw.Networking == nil {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(gw.Networking.Mode))
	return m == "docker" || m == "headscale" || m == "linode_vpc"
}

// NeedsInsecureWS reports whether OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 must
// be set in the gateway environment. Required whenever the gateway binds to a
// non-loopback address.
func NeedsInsecureWS(gw manifestdata.Gateway) bool {
	return NeedsLANBind(gw)
}

// NodeCompileCacheDir is an alias for [common.OpenclawNodeCompileCacheDir]
// (systemd drop-ins and EnsureNodeCompileCacheDir).
const NodeCompileCacheDir = common.OpenclawNodeCompileCacheDir

// StartupOptimEnv returns the environment variables openclaw recommends for
// fastest CLI startup on hosts that repeatedly respawn short-lived node
// processes (gateway crons, node exec, agents list, doctor, ...):
//
//   - NODE_COMPILE_CACHE persists V8-compiled module bytecode across runs so
//     the ts-node/typescript tax isn't paid on every invocation.
//   - OPENCLAW_NO_RESPAWN avoids the extra cold-start hit from openclaw's
//     self-respawn path (only useful for long-lived daemons; for one-shot
//     CLIs the respawn is pure overhead).
//
// These are always safe to set and materially improve cron/node exec latency
// on small VMs and ARM hosts, so we bake them into every systemd unit we
// manage rather than treating them as a low-power opt-in.
func StartupOptimEnv() map[string]string {
	return map[string]string{
		"NODE_COMPILE_CACHE":  NodeCompileCacheDir,
		"OPENCLAW_NO_RESPAWN": "1",
	}
}

// EnsureNodeCompileCacheDir creates the compile-cache directory on the remote
// host if it doesn't exist. /var/tmp is world-writable with sticky bit on
// every standard Linux distro so this does not need sudo; the directory ends
// up owned by whichever user runs the apply, which is the same user the
// openclaw systemd --user services run as. Idempotent.
func EnsureNodeCompileCacheDir(client *xssh.Client) error {
	script := fmt.Sprintf(`set -euo pipefail
mkdir -p %s
`, NodeCompileCacheDir)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("ensure compile cache dir: %w\n%s", err, out)
	}
	return nil
}

// DesiredBind returns "lan" or "loopback" for the gateway.
func DesiredBind(gw manifestdata.Gateway) string {
	if NeedsLANBind(gw) {
		return "lan"
	}
	return "loopback"
}

// GenerateToken creates a random 32-byte hex token suitable for
// gateway.auth.token. Hex encoding is URL- and shell-safe.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate gateway token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ReadToken reads the gateway auth token directly from
// ~/.openclaw/openclaw.json via SFTP, parsing gateway.auth.token from the
// JSON. Returns ("", nil) when the config file doesn't exist or the token
// field is absent/empty.
func ReadToken(client *xssh.Client, home string) (string, error) {
	raw, err := sshfile.ReadFile(client, home+"/"+configFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse openclaw config: %w", err)
	}
	return cfg.Gateway.Auth.Token, nil
}

// ConfigExists checks whether ~/.openclaw/openclaw.json exists via SFTP.
func ConfigExists(client *xssh.Client, home string) (bool, error) {
	_, err := sshfile.ReadFile(client, home+"/"+configFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ResolveHome determines the SSH user's home directory on the remote host.
func ResolveHome(client *xssh.Client) (string, error) {
	out, err := bash.RunOutput(client, `echo "$HOME"`)
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	h := strings.TrimSpace(out)
	if h == "" {
		return "", fmt.Errorf("resolve home: empty $HOME")
	}
	return h, nil
}

// HealthCheck polls the remote host until port 18789 is listening or
// retries are exhausted.
func HealthCheck(client *xssh.Client, retries int, delay time.Duration) error {
	if retries <= 0 {
		retries = healthRetries
	}
	if delay <= 0 {
		delay = healthDelay
	}
	for i := 0; i < retries; i++ {
		out, err := bash.RunOutput(client, `ss -tlnp 2>/dev/null | grep -q ':18789 ' && echo ok || echo no`)
		if err == nil && strings.TrimSpace(out) == "ok" {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("gateway not listening on :%d after %d attempts", gatewayPort, retries)
}

// RestartAndWait restarts the gateway systemd unit and waits until port
// 18789 is listening (more reliable than systemd is-active in containers).
func RestartAndWait(ctx context.Context, client *xssh.Client, userMode bool) error {
	if err := systemd.Restart(client, gatewayUnit, userMode); err != nil {
		return fmt.Errorf("restart %s: %w", gatewayUnit, err)
	}
	return HealthCheck(client, waitRetries, waitDelay)
}

// StartAndWait enables and starts the gateway systemd unit, then waits
// until port 18789 is listening.
func StartAndWait(ctx context.Context, client *xssh.Client, userMode bool) error {
	if err := systemd.EnableNow(client, gatewayUnit, userMode); err != nil {
		return fmt.Errorf("enable+start %s: %w", gatewayUnit, err)
	}
	return HealthCheck(client, waitRetries, waitDelay)
}

// ReadConfigValue reads a single openclaw config value from the remote host.
// Returns the trimmed output of `openclaw config get <key>`, or "" on error.
func ReadConfigValue(client *xssh.Client, key string) (string, error) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+fmt.Sprintf(
		`openclaw config get %s 2>/dev/null || echo "__missing__"`, key))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(out)
	if v == "__missing__" {
		return "", nil
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Device pairing
// ---------------------------------------------------------------------------

// ListDevices returns the gateway's current pending + paired device
// view. It prefers `openclaw devices list --json` when that succeeds,
// and falls back to parsing the daemon's on-disk state files when the
// CLI times out.
//
// The CLI fallback is load-bearing on resource-constrained hosts.
// Node.js on a 1-vCPU Linode (g6-standard-1) spends ~8–12 s just
// loading the openclaw CLI bundle; that blocks the event loop past
// the CLI's hard-coded 10 s gateway handshake timer and yields a
// spurious "gateway timeout after 10000ms" even when the daemon is
// healthy and reachable on 127.0.0.1:18789. The daemon's own state
// files (`~/.openclaw/nodes/pending.json`,
// `~/.openclaw/devices/paired.json`) are written synchronously by
// the daemon as pairings move through states, so reading them via
// SSH is a CLI-free, CPU-free path to the same information.
//
// On any error (SSH failure, unparseable JSON) we return an empty
// DeviceList so callers can retry without having to special-case nil.
func ListDevices(client *xssh.Client) (*DeviceList, error) {
	if dl, ok := listDevicesViaCLI(client); ok {
		return dl, nil
	}
	return listDevicesFromDisk(client), nil
}

// listDevicesViaCLI attempts the `openclaw devices list --json` path.
// ok=false signals the caller should fall back to the on-disk view:
// either the SSH exec errored, the output was empty (CLI timeout,
// which we explicitly suppress with 2>/dev/null), or the JSON was
// unparseable.
func listDevicesViaCLI(client *xssh.Client) (*DeviceList, bool) {
	out, err := bash.RunOutput(client, common.OpenclawCLIPreamble()+`openclaw devices list --json 2>/dev/null`)
	if err != nil {
		return nil, false
	}
	raw := strings.TrimSpace(out)
	if raw == "" {
		return nil, false
	}
	var dl DeviceList
	if err := json.Unmarshal([]byte(raw), &dl); err != nil {
		return nil, false
	}
	return &dl, true
}

// listDevicesFromDisk reads the daemon's pending + paired state files
// directly. Shape of the files (observed on openclaw 2026.4.x):
//
//   ~/.openclaw/nodes/pending.json
//     { "<requestId>": { "requestId": ..., "displayName": ..., ... } }
//
//   ~/.openclaw/devices/paired.json
//     { "<deviceId>":  { "deviceId": ..., "displayName": ...,
//                        "clientMode": "node"|"cli", "role": ..., ... } }
//
// Missing / empty files are treated as "{}" — a fresh install with
// no pairings yet is indistinguishable from a transient read miss,
// and callers already handle both cases by retrying.
func listDevicesFromDisk(client *xssh.Client) *DeviceList {
	dl := &DeviceList{}

	if raw, ok := readDaemonStateFile(client, `~/.openclaw/nodes/pending.json`); ok {
		var m map[string]struct {
			RequestID   string `json:"requestId"`
			DisplayName string `json:"displayName"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			for k, v := range m {
				rid := v.RequestID
				if rid == "" {
					rid = k
				}
				dl.Pending = append(dl.Pending, PendingDevice{
					RequestID:   rid,
					DisplayName: v.DisplayName,
					Role:        "node",
				})
			}
		}
	}

	if raw, ok := readDaemonStateFile(client, `~/.openclaw/devices/paired.json`); ok {
		var m map[string]struct {
			DisplayName string `json:"displayName"`
			ClientMode  string `json:"clientMode"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			for _, v := range m {
				dl.Paired = append(dl.Paired, PairedDevice{
					DisplayName: v.DisplayName,
					ClientMode:  v.ClientMode,
				})
			}
		}
	}

	return dl
}

// readDaemonStateFile cats a path under the agent user's home and
// returns (trimmed_content, ok). ok=false when SSH errored or the
// file was absent/empty; callers should treat that as "no entries".
func readDaemonStateFile(client *xssh.Client, path string) (string, bool) {
	out, err := bash.RunOutput(client, fmt.Sprintf(`cat %s 2>/dev/null || true`, path))
	if err != nil {
		return "", false
	}
	raw := strings.TrimSpace(out)
	if raw == "" || raw == "{}" {
		return "", false
	}
	return raw, true
}

// HasPairedLocalDevice reports whether any device in the paired list exists,
// indicating the local CLI has been approved for WebSocket access.
func HasPairedLocalDevice(dl *DeviceList) bool {
	return len(dl.Paired) > 0
}

// ApproveDevice approves a pending device by its requestId.
// It uses the gateway token to authenticate, bypassing the CLI device's limited scope.
func ApproveDevice(client *xssh.Client, requestID string) error {
	token := common.ReadGatewayAuthToken(client)

	var script string
	if token != "" {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw devices approve %q --token %q`, requestID, token)
	} else {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw devices approve %q`, requestID)
	}
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("approve device %s: %w\n%s", requestID, err, out)
	}
	return nil
}

// ApproveNode approves a pending node surface request by its requestId.
// It uses the gateway token to authenticate, which now has sufficient scope
// since OpenClaw 2026.5.x patched the operator.admin requirement.
func ApproveNode(client *xssh.Client, requestID string) error {
	token := common.ReadGatewayAuthToken(client)

	var script string
	if token != "" {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw nodes approve %q --token %q`, requestID, token)
	} else {
		script = common.OpenclawCLIPreamble() + fmt.Sprintf(`openclaw nodes approve %q`, requestID)
	}
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("approve node %s: %w\n%s", requestID, err, out)
	}
	return nil
}


