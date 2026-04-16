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

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/systemd"
	xssh "golang.org/x/crypto/ssh"
)

const (
	gatewayUnit     = "openclaw-gateway"
	gatewayPort     = 18789
	configFile = ".openclaw/openclaw.json"
	healthRetries   = 10
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

// RestartAndWait restarts the gateway systemd unit and waits until it is
// active. userMode controls --user vs sudo systemctl.
func RestartAndWait(ctx context.Context, client *xssh.Client, userMode bool) error {
	if err := systemd.Restart(client, gatewayUnit, userMode); err != nil {
		return fmt.Errorf("restart %s: %w", gatewayUnit, err)
	}
	return systemd.WaitActive(ctx, client, gatewayUnit, userMode, waitRetries, waitDelay)
}

// StartAndWait enables and starts the gateway systemd unit, then waits
// until it is active.
func StartAndWait(ctx context.Context, client *xssh.Client, userMode bool) error {
	if err := systemd.EnableNow(client, gatewayUnit, userMode); err != nil {
		return fmt.Errorf("enable+start %s: %w", gatewayUnit, err)
	}
	return systemd.WaitActive(ctx, client, gatewayUnit, userMode, waitRetries, waitDelay)
}

// ReadConfigValue reads a single openclaw config value from the remote host.
// Returns the trimmed output of `openclaw config get <key>`, or "" on error.
func ReadConfigValue(client *xssh.Client, key string) (string, error) {
	out, err := bash.RunOutput(client, fmt.Sprintf(
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

// DeviceList is the JSON structure returned by `openclaw devices list --json`.
type DeviceList struct {
	Pending []PendingDevice `json:"pending"`
	Paired  []PairedDevice  `json:"paired"`
}

// PendingDevice is a device awaiting approval.
type PendingDevice struct {
	RequestID   string `json:"requestId"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// PairedDevice is an approved device.
type PairedDevice struct {
	DisplayName string `json:"displayName"`
	ClientMode  string `json:"clientMode"`
}

// ListDevices runs `openclaw devices list --json` on the remote host and
// parses the result. Returns an empty DeviceList (not nil) on any failure.
func ListDevices(client *xssh.Client) (*DeviceList, error) {
	out, err := bash.RunOutput(client, `openclaw devices list --json 2>/dev/null || echo "{}"`)
	if err != nil {
		return &DeviceList{}, nil
	}
	raw := strings.TrimSpace(out)
	var dl DeviceList
	if err := json.Unmarshal([]byte(raw), &dl); err != nil {
		return &DeviceList{}, nil
	}
	return &dl, nil
}

// HasPairedLocalDevice reports whether any device in the paired list exists,
// indicating the local CLI has been approved for WebSocket access.
func HasPairedLocalDevice(dl *DeviceList) bool {
	return len(dl.Paired) > 0
}

// ApproveDevice approves a pending device by its requestId.
func ApproveDevice(client *xssh.Client, requestID string) error {
	script := fmt.Sprintf(`openclaw devices approve %q`, requestID)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("approve device %s: %w\n%s", requestID, err, out)
	}
	return nil
}
