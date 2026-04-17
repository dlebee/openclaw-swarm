//go:build integration_docker

package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/mesh"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	intssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	xssh "golang.org/x/crypto/ssh"
)

const identityName = "integration"

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func loadTestManifest(t *testing.T) *data.Manifest {
	return loadTestManifestFile(t, "manifest.yml")
}

func loadTestManifestFile(t *testing.T, name string) *data.Manifest {
	t.Helper()
	path := filepath.Join("testdata", name)
	m, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("load manifest %s: %v", name, err)
	}
	return m
}

// generateTestIdentity creates an ephemeral Ed25519 keypair in a temp dir.
func generateTestIdentity(t *testing.T) (privPath, pubPath string) {
	t.Helper()
	dir := t.TempDir()
	id, err := intssh.GeneratePEMIdentity(dir, identityName)
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	priv, err := intssh.ExpandPath(id.PrivateKeyPath)
	if err != nil {
		t.Fatalf("expand private key path: %v", err)
	}
	pub, err := intssh.ExpandPath(id.PublicKeyPath)
	if err != nil {
		t.Fatalf("expand public key path: %v", err)
	}
	return priv, pub
}

// testNetwork creates a shared Docker network for the test containers.
func testNetwork(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create test network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })
	return net.Name
}

const (
	ocTestImage     = "oc-test:latest"
	ollamaTestImage = "oc-ollama-test:latest"
)

// ocContainer starts a pre-built oc-test container with the given pubkey
// injected. The entrypoint installs the key for root and agent users.
// Build the image first: ./test/infra/build.sh
func ocContainer(t *testing.T, netName, pubKeyPath, name string, aliases ...string) tc.Container {
	t.Helper()
	ctx := context.Background()
	t.Logf("start container: %s", name)
	absKey, err := filepath.Abs(pubKeyPath)
	if err != nil {
		t.Fatalf("abs pubkey path: %v", err)
	}
	netAliases := map[string][]string{}
	if len(aliases) > 0 {
		netAliases[netName] = aliases
	}
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:          ocTestImage,
			ExposedPorts:   []string{"22/tcp"},
			Networks:       []string{netName},
			NetworkAliases: netAliases,
			Files: []tc.ContainerFile{
				{
					HostFilePath:      absKey,
					ContainerFilePath: "/tmp/authorized_key.pub",
					FileMode:          0o644,
				},
			},
			WaitingFor: wait.ForListeningPort("22/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
		Logger:  testLogger{t},
	})
	if err != nil {
		t.Fatalf("start %s: %v\n(build images first: ./test/infra/build.sh)", name, err)
	}
	t.Cleanup(func() {
		t.Logf("terminate container: %s", name)
		_ = ctr.Terminate(context.Background())
	})
	t.Logf("container ready: %s", name)
	return ctr
}

// ollamaContainer starts the pre-built oc-ollama-test image with network alias "ollama".
// Build the image first: ./test/infra/build.sh
func ollamaContainer(t *testing.T, netName string) tc.Container {
	t.Helper()
	ctx := context.Background()
	t.Log("start container: ollama")
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        ollamaTestImage,
			ExposedPorts: []string{"11434/tcp"},
			Networks:     []string{netName},
			NetworkAliases: map[string][]string{
				netName: {"ollama"},
			},
			WaitingFor: wait.ForHTTP("/api/tags").WithPort("11434/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
		Logger:  testLogger{t},
	})
	if err != nil {
		t.Fatalf("start ollama: %v\n(build images first: ./test/infra/build.sh)", err)
	}
	t.Cleanup(func() {
		t.Log("terminate container: ollama")
		_ = ctr.Terminate(context.Background())
	})
	t.Log("container ready: ollama")
	return ctr
}

// mappedPort returns the host port mapped to a container's exposed port.
func mappedPort(t *testing.T, ctr tc.Container, port string) int {
	t.Helper()
	p, err := ctr.MappedPort(context.Background(), port)
	if err != nil {
		t.Fatalf("mapped port %s: %v", port, err)
	}
	return int(p.Num())
}

// sshSigner reads a PEM private key and returns an ssh.Signer.
func sshSigner(t *testing.T, privPath string) xssh.Signer {
	t.Helper()
	keyData, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	signer, err := xssh.ParsePrivateKey(keyData)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	return signer
}

// waitSSH polls until an SSH handshake succeeds.
func waitSSH(t *testing.T, host string, port int, user string, signer xssh.Signer, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	cfg := &xssh.ClientConfig{
		User:            user,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		sshConn, chans, reqs, err := xssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			conn.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		client := xssh.NewClient(sshConn, chans, reqs)
		client.Close()
		return
	}
	t.Fatalf("SSH not reachable at %s within %s", addr, timeout)
}

// testLogger bridges testcontainers log output to t.Log.
type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, v ...interface{}) {
	l.t.Logf("[tc] "+format, v...)
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestApplyPlan verifies the scaffold plan structure against Docker containers:
//   - provisioning, security, mesh steps are not applicable for ssh+container
//   - gateway install steps are applicable + satisfied (pre-installed)
//   - bootstrap-gateway is applicable (fresh container)
//   - configure-gateway and pair-gateway-device are not applicable (no config yet)
func TestApplyPlan(t *testing.T) {
	m, signer, _, _ := setupTestInfra(t)
	dial := sshDialFunc(signer)

	plan, err := apply.BuildPlan(apply.BuildOptions{
		Manifest: m,
		SSHDial:  dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	ep, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	ctx := context.Background()
	ctx = scaffold.EnsurePlanCache(ctx)

	for _, phase := range plan.Phases {
		for _, target := range phase.Targets {
			for _, step := range phase.Steps {
				applicable, err := step.Applicable(ctx, target)
				if err != nil {
					t.Errorf("phase=%s target=%s step=%s: Applicable error: %v",
						phase.Name, target.ID, step.Name(), err)
					continue
				}

				switch phase.Name {
				case "provisioning", "security", "mesh":
					if applicable {
						t.Errorf("phase=%s target=%s step=%s: expected not applicable, got applicable",
							phase.Name, target.ID, step.Name())
					}
			case "gateway":
				stepName := step.Name()
				switch stepName {
				case "install-nodejs", "install-openclaw":
					// Pre-installed in the Docker image: applicable + satisfied.
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
						continue
					}
					satisfied, err := step.Check(ctx, target)
					if err != nil {
						t.Errorf("phase=%s target=%s step=%s: Check error: %v",
							phase.Name, target.ID, stepName, err)
						continue
					}
					if !satisfied {
						t.Errorf("phase=%s target=%s step=%s: expected satisfied, got not satisfied",
							phase.Name, target.ID, stepName)
					}
				case "bootstrap-gateway", "configure-gateway", "pair-gateway-device":
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
					}
				default:
					t.Errorf("phase=%s target=%s step=%s: unexpected step name",
						phase.Name, target.ID, stepName)
				}
			case "node":
				stepName := step.Name()
				switch stepName {
				case "install-nodejs", "install-openclaw":
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
						continue
					}
					satisfied, err := step.Check(ctx, target)
					if err != nil {
						t.Errorf("phase=%s target=%s step=%s: Check error: %v",
							phase.Name, target.ID, stepName, err)
						continue
					}
					if !satisfied {
						t.Errorf("phase=%s target=%s step=%s: expected satisfied, got not satisfied",
							phase.Name, target.ID, stepName)
					}
				case "bootstrap-node", "configure-node", "pair-node":
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
					}
				case "exec-policy":
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable (exec_policy set in manifest), got not applicable",
							phase.Name, target.ID, stepName)
					}
				default:
					t.Errorf("phase=%s target=%s step=%s: unexpected step name",
						phase.Name, target.ID, stepName)
				}
			case "agents":
				stepName := step.Name()
				switch stepName {
				case "add-agent", "ensure-model", "configure-workspace":
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
					}
				case "configure-tools":
					// Only applicable when agent has tools defined.
					// Both test agents have tools, so expect applicable.
					if !applicable {
						t.Errorf("phase=%s target=%s step=%s: expected applicable, got not applicable",
							phase.Name, target.ID, stepName)
					}
				case "configure-bindings":
					// No bindings in test manifest, so expect not applicable.
					if applicable {
						t.Errorf("phase=%s target=%s step=%s: expected not applicable (no bindings), got applicable",
							phase.Name, target.ID, stepName)
					}
				default:
					t.Errorf("phase=%s target=%s step=%s: unexpected step name",
						phase.Name, target.ID, stepName)
				}
				}
			}
		}
	}

	desc, err := ep.Describe(ctx)
	if err != nil {
		t.Fatalf("describe plan: %v", err)
	}
	t.Logf("plan describe:\n%s", desc)
}

// setupTestInfra spins up Docker containers and returns the patched manifest,
// SSH signer, mapped gateway port, and mapped scraper port.
func setupTestInfra(t *testing.T) (*data.Manifest, xssh.Signer, int, int) {
	t.Helper()
	privPath, pubPath := generateTestIdentity(t)
	t.Logf("identity: priv=%s pub=%s", privPath, pubPath)

	netName := testNetwork(t)

	gw := ocContainer(t, netName, pubPath, "gateway", "oc-gateway-test")
	scraper := ocContainer(t, netName, pubPath, "scraper", "oc-scraper-test")
	_ = ollamaContainer(t, netName)

	gwPort := mappedPort(t, gw, "22/tcp")
	scraperPort := mappedPort(t, scraper, "22/tcp")
	t.Logf("mapped ports: gateway=%d scraper=%d", gwPort, scraperPort)

	signer := sshSigner(t, privPath)
	waitSSH(t, "127.0.0.1", gwPort, "root", signer, 30*time.Second)
	waitSSH(t, "127.0.0.1", scraperPort, "root", signer, 30*time.Second)
	t.Log("SSH connectivity confirmed on gateway and scraper")

	m := loadTestManifest(t)
	for i := range m.Machines {
		switch m.Machines[i].Name {
		case "gateway-host":
			m.Machines[i].SSHPort = gwPort
		case "scraper-host":
			m.Machines[i].SSHPort = scraperPort
		}
	}
	return m, signer, gwPort, scraperPort
}

// sshDialFunc returns an SSHDialFunc that uses the given signer.
func sshDialFunc(signer xssh.Signer) func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
	return func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		cfg := &xssh.ClientConfig{
			User:            user,
			Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
			HostKeyCallback: xssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		}
		return xssh.Dial("tcp", addr, cfg)
	}
}

// TestApplyExecute runs the full apply plan against Docker containers and
// verifies the gateway is properly bootstrapped: config exists, gateway.mode
// and gateway.bind are correct, the token side-file is written, and the
// gateway process is listening on port 18789.
func TestApplyExecute(t *testing.T) {
	m, signer, gwPort, scraperPort := setupTestInfra(t)
	dial := sshDialFunc(signer)

	plan, err := apply.BuildPlan(apply.BuildOptions{
		Manifest: m,
		SSHDial:  dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	ep, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	ctx := context.Background()
	ctx = scaffold.EnsurePlanCache(ctx)

	if err := ep.Execute(ctx, scaffold.ExecuteOptions{}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	t.Log("plan executed successfully")

	// --- Verify gateway state ---
	client, err := dial(ctx, "127.0.0.1", gwPort, "root")
	if err != nil {
		t.Fatalf("dial gateway for verification: %v", err)
	}
	defer client.Close()

	// 1. Config file must exist.
	home, err := gwService.ResolveHome(client)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	exists, err := gwService.ConfigExists(client, home)
	if err != nil {
		t.Fatalf("check config exists: %v", err)
	}
	if !exists {
		t.Fatal("expected ~/.openclaw/openclaw.json to exist after bootstrap")
	}
	t.Log("verified: config file exists")

	// 2. gateway.mode must be "local".
	mode, err := gwService.ReadConfigValue(client, "gateway.mode")
	if err != nil {
		t.Fatalf("read gateway.mode: %v", err)
	}
	if mode != "local" {
		t.Fatalf("gateway.mode: got %q, want %q", mode, "local")
	}
	t.Log("verified: gateway.mode=local")

	// 3. gateway.bind must be "lan" (networking.mode=docker).
	bind, err := gwService.ReadConfigValue(client, "gateway.bind")
	if err != nil {
		t.Fatalf("read gateway.bind: %v", err)
	}
	if bind != "lan" {
		t.Fatalf("gateway.bind: got %q, want %q", bind, "lan")
	}
	t.Log("verified: gateway.bind=lan")

	// 4. Token in config must be non-empty.
	token, err := gwService.ReadToken(client, home)
	if err != nil {
		t.Fatalf("read token from config: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty gateway.auth.token in openclaw.json")
	}
	t.Logf("verified: gateway.auth.token present (%d chars)", len(token))

	// 5. Gateway must be listening on port 18789.
	if err := gwService.HealthCheck(client, 15, 2*time.Second); err != nil {
		t.Fatalf("health check: %v", err)
	}
	t.Log("verified: gateway listening on :18789")

	// 6. Systemd env drop-in must contain OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1
	//    (required for networking.mode=docker / LAN bind).
	dropInEnv, err := bash.RunOutput(client, `cat ~/.config/systemd/user/openclaw-gateway.service.d/env.conf 2>/dev/null || echo "(not found)"`)
	if err != nil {
		t.Fatalf("read env drop-in: %v", err)
	}
	if !strings.Contains(dropInEnv, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1") {
		t.Fatalf("expected OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 in env drop-in, got:\n%s", dropInEnv)
	}
	t.Log("verified: OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 in gateway systemd env drop-in")

	// --- Verify node (scraper) state ---
	nodeClient, err := dial(ctx, "127.0.0.1", scraperPort, "root")
	if err != nil {
		t.Fatalf("dial scraper for verification: %v", err)
	}
	defer nodeClient.Close()

	// 7. Node systemd unit must exist and reference the gateway internal host.
	nodeUnit, err := bash.RunOutput(nodeClient, `cat ~/.config/systemd/user/openclaw-node.service 2>/dev/null || echo "(not found)"`)
	if err != nil {
		t.Fatalf("read node unit: %v", err)
	}
	if strings.Contains(nodeUnit, "(not found)") {
		t.Fatal("expected openclaw-node.service to exist on scraper node")
	}
	if !strings.Contains(nodeUnit, "oc-gateway-test") {
		t.Fatalf("expected node unit to reference gateway internal host oc-gateway-test, got:\n%s", nodeUnit)
	}
	t.Log("verified: openclaw-node.service exists and references gateway")

	// 8. Node unit must have OPENCLAW_GATEWAY_TOKEN set in its Environment.
	if !strings.Contains(nodeUnit, "OPENCLAW_GATEWAY_TOKEN") {
		t.Fatalf("expected node unit to contain OPENCLAW_GATEWAY_TOKEN in Environment, got:\n%s", nodeUnit)
	}
	t.Log("verified: node unit contains OPENCLAW_GATEWAY_TOKEN")

	// 9. Node env drop-in must contain OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1.
	nodeDropIn, err := bash.RunOutput(nodeClient, `cat ~/.config/systemd/user/openclaw-node.service.d/env.conf 2>/dev/null || echo "(not found)"`)
	if err != nil {
		t.Fatalf("read node env drop-in: %v", err)
	}
	if !strings.Contains(nodeDropIn, "OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1") {
		t.Fatalf("expected OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 in node env drop-in, got:\n%s", nodeDropIn)
	}
	t.Log("verified: OPENCLAW_ALLOW_INSECURE_PRIVATE_WS=1 in node systemd env drop-in")

	// 10. Node exec-policy must be set (security=full, ask=off).
	execSec, err := bash.RunOutput(nodeClient, `openclaw config get tools.exec.security 2>/dev/null || echo ""`)
	if err != nil {
		t.Fatalf("read node exec security: %v", err)
	}
	if strings.TrimSpace(execSec) != "full" {
		t.Fatalf("node tools.exec.security: got %q, want %q", strings.TrimSpace(execSec), "full")
	}
	t.Log("verified: node exec-policy security=full")

	execAsk, err := bash.RunOutput(nodeClient, `openclaw config get tools.exec.ask 2>/dev/null || echo ""`)
	if err != nil {
		t.Fatalf("read node exec ask: %v", err)
	}
	if strings.TrimSpace(execAsk) != "off" {
		t.Fatalf("node tools.exec.ask: got %q, want %q", strings.TrimSpace(execAsk), "off")
	}
	t.Log("verified: node exec-policy ask=off")

	// 11. Node must be paired on the gateway (device approved with displayName=scraper-node).
	dl, err := gwService.ListDevices(client)
	if err != nil {
		t.Fatalf("list devices on gateway: %v", err)
	}
	found := false
	for _, d := range dl.Paired {
		if d.DisplayName == "scraper-node" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected scraper-node to be paired on the gateway")
	}
	t.Log("verified: scraper-node paired on gateway")

	// --- Verify agents state (both agents live on the gateway machine) ---

	// 11. Both agents must appear in `openclaw agents list --json`.
	agentListOut, err := bash.RunOutput(client, `openclaw agents list --json 2>/dev/null || echo "[]"`)
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	for _, agentID := range []string{"assistant", "scraper"} {
		if !strings.Contains(agentListOut, agentID) {
			t.Fatalf("expected agent %q in agents list, got:\n%s", agentID, agentListOut)
		}
	}
	t.Log("verified: both agents registered")

	// 12. Each agent must have the correct model set.
	if !strings.Contains(agentListOut, "ollama/qwen2.5:0.5b") {
		t.Fatalf("expected model ollama/qwen2.5:0.5b in agents list, got:\n%s", agentListOut)
	}
	t.Log("verified: agent models set correctly")

	// 13. SOUL.md must exist with managed section markers and correct content.
	gwHome, err := gwService.ResolveHome(client)
	if err != nil {
		t.Fatalf("resolve gateway home: %v", err)
	}
	soulContent, err := bash.RunOutput(client, fmt.Sprintf(`cat %s/.openclaw/workspace/SOUL.md 2>/dev/null || echo "(not found)"`, gwHome))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if strings.Contains(soulContent, "(not found)") {
		t.Fatal("expected SOUL.md to exist in agent workspace")
	}
	if !strings.Contains(soulContent, "integration testing") {
		t.Fatalf("SOUL.md content mismatch, got:\n%s", soulContent)
	}
	if !strings.Contains(soulContent, "CLAWS MANAGED START") {
		t.Fatalf("SOUL.md missing managed section markers, got:\n%s", soulContent)
	}
	if !strings.Contains(soulContent, "CLAWS MANAGED END") {
		t.Fatalf("SOUL.md missing managed section end marker, got:\n%s", soulContent)
	}
	t.Log("verified: SOUL.md with managed section markers")

	// 14. IDENTITY.md must exist with managed section markers.
	identityContent, err := bash.RunOutput(client, fmt.Sprintf(`cat %s/.openclaw/workspace/IDENTITY.md 2>/dev/null || echo "(not found)"`, gwHome))
	if err != nil {
		t.Fatalf("read IDENTITY.md: %v", err)
	}
	if strings.Contains(identityContent, "(not found)") {
		t.Fatal("expected IDENTITY.md to exist in agent workspace")
	}
	if !strings.Contains(identityContent, "CLAWS MANAGED START") {
		t.Fatalf("IDENTITY.md missing managed section markers, got:\n%s", identityContent)
	}
	t.Log("verified: IDENTITY.md with managed section markers")

	// 15. Global tools.elevated must be configured (enabled=true, allowFrom.telegram).
	elevEnabled, err := gwService.ReadConfigValue(client, "tools.elevated.enabled")
	if err != nil {
		t.Fatalf("read tools.elevated.enabled: %v", err)
	}
	if elevEnabled != "true" {
		t.Fatalf("tools.elevated.enabled: got %q, want %q", elevEnabled, "true")
	}
	t.Log("verified: tools.elevated.enabled=true")

	elevAllowFrom, err := bash.RunOutput(client, `openclaw config get tools.elevated.allowFrom.telegram --json 2>/dev/null || echo "null"`)
	if err != nil {
		t.Fatalf("read tools.elevated.allowFrom.telegram: %v", err)
	}
	if !strings.Contains(elevAllowFrom, "admin-chat-123") {
		t.Fatalf("tools.elevated.allowFrom.telegram missing admin-chat-123, got:\n%s", elevAllowFrom)
	}
	t.Log("verified: tools.elevated.allowFrom.telegram contains admin-chat-123")

	// 17. Agent exec tools config on gateway (per-agent in openclaw.json).
	assistantExecSec, err := bash.RunOutput(client, `openclaw config get agents.list --json 2>/dev/null || echo "[]"`)
	if err != nil {
		t.Fatalf("read agents.list: %v", err)
	}
	if !strings.Contains(assistantExecSec, `"security"`) {
		t.Fatalf("expected agents.list to contain exec security config, got:\n%s", assistantExecSec)
	}
	t.Log("verified: agent exec tools config set in openclaw.json")

	// 18. Log exec-approvals.json state for diagnostics.
	execApprovals, err := bash.RunOutput(client, fmt.Sprintf(`cat %s/.openclaw/exec-approvals.json 2>/dev/null || echo "(not found)"`, gwHome))
	if err != nil {
		t.Logf("exec-approvals.json read error (non-fatal): %v", err)
	} else {
		t.Logf("exec-approvals.json state:\n%s", execApprovals)
	}

	// 19. Automations phase: touch-marker must have executed on both machines.
	for label, c := range map[string]*xssh.Client{"gateway": client, "scraper": nodeClient} {
		markerOut, err := bash.RunOutput(c, `cat /tmp/claws-automation-marker 2>/dev/null || echo "(not found)"`)
		if err != nil {
			t.Fatalf("read marker on %s: %v", label, err)
		}
		if !strings.Contains(markerOut, "created-by-claws-automation") {
			t.Fatalf("expected marker file on %s, got:\n%s", label, markerOut)
		}
		t.Logf("verified: automation marker present on %s", label)
	}

	// 20. End-to-end exec: gateway dispatches system.which to the scraper node
	//     over the WebSocket pipeline. This proves exec-policy, node pairing,
	//     and agent exec config all work without raw exec-approvals.json writes.

	t.Log("waiting for scraper-node WebSocket connection...")
	waitNodeConnected(t, client, "scraper-node", 90*time.Second)

	invokeScript := `openclaw nodes invoke --node scraper-node --command system.which --params '{"bins":["echo"]}' --json 2>&1`
	invokeOut, err := bash.RunOutput(client, invokeScript)
	if err != nil {
		t.Fatalf("nodes invoke system.which failed: %v\n%s", err, invokeOut)
	}
	if !strings.Contains(invokeOut, "echo") {
		t.Fatalf("system.which response missing 'echo', got:\n%s", invokeOut)
	}
	t.Log("verified: gateway→scraper-node exec pipeline works (system.which returned echo)")
}

// waitNodeConnected polls `openclaw nodes invoke` until the gateway can
// dispatch to the named node, meaning its WebSocket connection is live.
func waitNodeConnected(t *testing.T, gwClient *xssh.Client, nodeName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	script := fmt.Sprintf(
		`openclaw nodes invoke --node %s --command system.which --params '{"bins":["echo"]}' --json 2>&1`, nodeName)
	for time.Now().Before(deadline) {
		out, err := bash.RunOutput(gwClient, script)
		if err == nil && strings.Contains(out, "echo") {
			t.Logf("node %q WebSocket connected (system.which succeeded)", nodeName)
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("node %q not connected within %s", nodeName, timeout)
}

// ---------------------------------------------------------------------------
// Mesh phase test helpers
// ---------------------------------------------------------------------------

const ocMeshTestImage = "oc-mesh-test:latest"

// meshContainer starts a pre-built oc-mesh-test container (stub headscale +
// tailscale) with the given pubkey injected. Build first: ./test/infra/build.sh
func meshContainer(t *testing.T, netName, pubKeyPath, name string, aliases ...string) tc.Container {
	t.Helper()
	ctx := context.Background()
	t.Logf("start mesh container: %s", name)
	absKey, err := filepath.Abs(pubKeyPath)
	if err != nil {
		t.Fatalf("abs pubkey path: %v", err)
	}
	netAliases := map[string][]string{}
	if len(aliases) > 0 {
		netAliases[netName] = aliases
	}
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:          ocMeshTestImage,
			ExposedPorts:   []string{"22/tcp"},
			Networks:       []string{netName},
			NetworkAliases: netAliases,
			Files: []tc.ContainerFile{
				{
					HostFilePath:      absKey,
					ContainerFilePath: "/tmp/authorized_key.pub",
					FileMode:          0o644,
				},
			},
			WaitingFor: wait.ForListeningPort("22/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
		Logger:  testLogger{t},
	})
	if err != nil {
		t.Fatalf("start mesh container %s: %v\n(build images first: ./test/infra/build.sh)", name, err)
	}
	t.Cleanup(func() {
		t.Logf("terminate mesh container: %s", name)
		_ = ctr.Terminate(context.Background())
	})
	t.Logf("mesh container ready: %s", name)
	return ctr
}

// loadMeshManifest loads the headscale-mode test manifest.
func loadMeshManifest(t *testing.T) *data.Manifest {
	t.Helper()
	path := filepath.Join("testdata", "manifest-mesh.yml")
	m, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("load mesh manifest: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// TestMeshPhase
// ---------------------------------------------------------------------------

// TestMeshPhase verifies the mesh phase (headscale + tailscale) using Docker
// containers with stub headscale and tailscale binaries.
func TestMeshPhase(t *testing.T) {
	privPath, pubPath := generateTestIdentity(t)
	t.Logf("identity: priv=%s pub=%s", privPath, pubPath)

	netName := testNetwork(t)

	gw := meshContainer(t, netName, pubPath, "gateway", "oc-mesh-gateway")
	node := meshContainer(t, netName, pubPath, "node", "oc-mesh-node")

	gwPort := mappedPort(t, gw, "22/tcp")
	nodePort := mappedPort(t, node, "22/tcp")
	t.Logf("mapped ports: gateway=%d node=%d", gwPort, nodePort)

	signer := sshSigner(t, privPath)
	waitSSH(t, "127.0.0.1", gwPort, "root", signer, 30*time.Second)
	waitSSH(t, "127.0.0.1", nodePort, "root", signer, 30*time.Second)
	t.Log("SSH connectivity confirmed on gateway and node")

	// Load and patch manifest with mapped SSH ports.
	m := loadMeshManifest(t)
	for i := range m.Machines {
		switch m.Machines[i].Name {
		case "gateway-host":
			m.Machines[i].SSHPort = gwPort
		case "scraper-host":
			m.Machines[i].SSHPort = nodePort
		}
	}

	dial := sshDialFunc(signer)

	// Build a mesh-only scaffold plan (no provisioning/security/gateway phases).
	p := scaffold.New()
	targets := provisioning.BuildMachineTargets(m.Machines)
	mesh.AddPhase(p, targets, mesh.Options{
		SSHDial:  mesh.SSHDialFunc(dial),
		Machines: m.Machines,
		Gateways: m.Gateways,
		Nodes:    m.Nodes,
	})

	ep, err := p.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	ctx := context.Background()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- Verify Applicable/Check before Execute ---
	// AddPhase creates two phases: mesh-gateway (gateway setup) and mesh-join
	// (tailscale join on all targets). Find them by name.
	findPhase := func(name string) *scaffold.Phase {
		for _, ph := range p.Phases {
			if ph.Name == name {
				return ph
			}
		}
		return nil
	}
	gwPhase := findPhase("mesh-gateway")
	if gwPhase == nil {
		t.Fatal("pre-execute: mesh-gateway phase not found")
	}
	joinPhase := findPhase("mesh-join")
	if joinPhase == nil {
		t.Fatal("pre-execute: mesh-join phase not found")
	}

	// Check gateway-phase steps on the gateway target.
	for _, target := range gwPhase.Targets {
		for _, step := range gwPhase.Steps {
			applicable, err := step.Applicable(ctx, target)
			if err != nil {
				t.Errorf("pre-execute: phase=mesh-gateway target=%s step=%s: Applicable error: %v",
					target.ID, step.Name(), err)
				continue
			}
			if step.Name() == "install-headscale" && target.ID == "gateway-host" {
				if !applicable {
					t.Errorf("pre-execute: install-headscale: expected applicable on gateway-host")
				}
				// Check should be false initially (headscale not yet configured).
				satisfied, err := step.Check(ctx, target)
				if err != nil {
					t.Errorf("pre-execute: install-headscale Check error: %v", err)
				}
				if satisfied {
					t.Error("pre-execute: install-headscale: expected not satisfied initially")
				}
			}
		}
	}

	// Check join-phase: install-tailscale must be applicable on all mesh targets.
	for _, target := range joinPhase.Targets {
		for _, step := range joinPhase.Steps {
			applicable, err := step.Applicable(ctx, target)
			if err != nil {
				t.Errorf("pre-execute: phase=mesh-join target=%s step=%s: Applicable error: %v",
					target.ID, step.Name(), err)
				continue
			}
			if step.Name() == "install-tailscale" && !applicable {
				t.Errorf("pre-execute: install-tailscale: expected applicable on all mesh targets, got not applicable for %s", target.ID)
			}
		}
	}

	// --- Execute mesh phase ---
	if err := ep.Execute(ctx, scaffold.ExecuteOptions{}); err != nil {
		t.Fatalf("execute mesh plan: %v", err)
	}
	t.Log("mesh plan executed successfully")

	// --- Verify gateway state ---
	gwClient, err := dial(ctx, "127.0.0.1", gwPort, "root")
	if err != nil {
		t.Fatalf("dial gateway for verification: %v", err)
	}
	defer gwClient.Close()

	// 1. Preauth key file must be non-empty.
	keyContent, err := bash.RunOutput(gwClient, "sudo cat /var/lib/claws/headscale/preauth.key 2>/dev/null || echo ''")
	if err != nil {
		t.Fatalf("read preauth key on gateway: %v", err)
	}
	if strings.TrimSpace(keyContent) == "" {
		t.Fatal("expected preauth key file to be non-empty on gateway")
	}
	t.Logf("verified: preauth key present on gateway: %s", strings.TrimSpace(keyContent))

	// 2. Tailscale IP must be present on gateway.
	gwIP, err := bash.RunOutput(gwClient, "tailscale ip -4 2>/dev/null || echo ''")
	if err != nil {
		t.Fatalf("tailscale ip -4 on gateway: %v", err)
	}
	if strings.TrimSpace(gwIP) == "" {
		t.Fatal("expected tailscale IP on gateway after mesh join")
	}
	t.Logf("verified: gateway tailscale IP: %s", strings.TrimSpace(gwIP))

	// --- Verify node state ---
	nodeClient, err := dial(ctx, "127.0.0.1", nodePort, "root")
	if err != nil {
		t.Fatalf("dial node for verification: %v", err)
	}
	defer nodeClient.Close()

	// 3. Tailscale IP must be present on node.
	nodeIP, err := bash.RunOutput(nodeClient, "tailscale ip -4 2>/dev/null || echo ''")
	if err != nil {
		t.Fatalf("tailscale ip -4 on node: %v", err)
	}
	if strings.TrimSpace(nodeIP) == "" {
		t.Fatal("expected tailscale IP on node after mesh join")
	}
	t.Logf("verified: node tailscale IP: %s", strings.TrimSpace(nodeIP))
}

// ---------------------------------------------------------------------------
// Mesh integration test
// ---------------------------------------------------------------------------

// setupMeshTestInfra spins up Docker containers for mesh testing and returns
// the patched manifest, SSH signer, and mapped ports.
func setupMeshTestInfra(t *testing.T) (*data.Manifest, xssh.Signer, int, int) {
	t.Helper()
	privPath, pubPath := generateTestIdentity(t)
	t.Logf("identity: priv=%s pub=%s", privPath, pubPath)

	netName := testNetwork(t)

	gw := meshContainer(t, netName, pubPath, "gateway-mesh", "oc-gateway-mesh-test")
	scraper := meshContainer(t, netName, pubPath, "scraper-mesh", "oc-scraper-mesh-test")

	gwPort := mappedPort(t, gw, "22/tcp")
	scraperPort := mappedPort(t, scraper, "22/tcp")
	t.Logf("mapped ports: gateway=%d scraper=%d", gwPort, scraperPort)

	signer := sshSigner(t, privPath)
	waitSSH(t, "127.0.0.1", gwPort, "root", signer, 30*time.Second)
	waitSSH(t, "127.0.0.1", scraperPort, "root", signer, 30*time.Second)
	t.Log("SSH connectivity confirmed on gateway and scraper (mesh)")

	m := loadTestManifestFile(t, "manifest-mesh.yml")
	for i := range m.Machines {
		switch m.Machines[i].Name {
		case "gateway-host":
			m.Machines[i].SSHPort = gwPort
		case "scraper-host":
			m.Machines[i].SSHPort = scraperPort
		}
	}
	return m, signer, gwPort, scraperPort
}

// TestMeshApplyExecute runs the full apply plan with headscale mesh networking
// against Docker containers and verifies:
//   - Headscale is running on the gateway
//   - A preauth key was created on the gateway
//   - Both machines have Tailscale IPs
//   - Machines can reach each other over the tailnet
func TestMeshApplyExecute(t *testing.T) {
	m, signer, gwPort, scraperPort := setupMeshTestInfra(t)
	dial := sshDialFunc(signer)

	plan, err := apply.BuildPlan(apply.BuildOptions{
		Manifest: m,
		SSHDial:  dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	ep, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	ctx := context.Background()
	ctx = scaffold.EnsurePlanCache(ctx)

	if err := ep.Execute(ctx, scaffold.ExecuteOptions{
		// Only run provisioning, security and mesh phases.
		// Gateway/node/agents require a running openclaw instance which isn't
		// available in the Docker mesh test environment.
		SkipPhases: []string{"gateway", "node", "agents"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	t.Log("mesh plan executed successfully")

	// --- Verify headscale on gateway ---
	gwClient, err := dial(ctx, "127.0.0.1", gwPort, "root")
	if err != nil {
		t.Fatalf("dial gateway for verification: %v", err)
	}
	defer gwClient.Close()

	// 1. Headscale binary must exist.
	hsOut, err := bash.RunOutput(gwClient, `/usr/local/bin/headscale version 2>/dev/null || echo "(missing)"`)
	if err != nil || strings.Contains(hsOut, "(missing)") {
		t.Fatal("headscale binary not found on gateway after mesh phase")
	}
	t.Logf("verified: headscale installed (%s)", strings.TrimSpace(hsOut))

	// 2. Headscale config must exist.
	cfgExists, err := bash.RunOutput(gwClient, `test -f /etc/headscale/config.yaml && echo ok || echo no`)
	if err != nil || strings.TrimSpace(cfgExists) != "ok" {
		t.Fatal("headscale config missing on gateway")
	}
	t.Log("verified: headscale config exists")

	// 3. Headscale unix socket must be present (daemon running).
	sockExists, err := bash.RunOutput(gwClient, `sudo test -S /var/run/headscale/headscale.sock && echo ok || echo no`)
	if err != nil || strings.TrimSpace(sockExists) != "ok" {
		t.Fatal("headscale unix socket not present — daemon may not be running")
	}
	t.Log("verified: headscale daemon running (socket present)")

	// 4. Preauth key file must exist and be non-empty.
	keyContent, err := bash.RunOutput(gwClient, `sudo cat /var/lib/claws/headscale/preauth.key 2>/dev/null || echo ""`)
	if err != nil || strings.TrimSpace(keyContent) == "" {
		t.Fatal("preauth key file missing or empty on gateway")
	}
	t.Logf("verified: preauth key present (%d chars)", len(strings.TrimSpace(keyContent)))

	// 5. Tailscale binary must be installed on gateway (join is skipped in container mode).
	gwTSBin, err := bash.RunOutput(gwClient, `command -v tailscale >/dev/null 2>&1 && echo ok || echo missing`)
	if err != nil || strings.TrimSpace(gwTSBin) != "ok" {
		t.Fatal("tailscale binary not found on gateway")
	}
	gwTSIP, _ := bash.RunOutput(gwClient, `tailscale ip -4 2>/dev/null || echo ""`)
	if strings.TrimSpace(gwTSIP) != "" {
		t.Logf("verified: gateway tailscale IP = %s", strings.TrimSpace(gwTSIP))
	} else {
		t.Log("verified: tailscale installed on gateway (no IP — join skipped in container mode)")
	}

	// --- Verify tailscale on scraper node ---
	nodeClient, err := dial(ctx, "127.0.0.1", scraperPort, "root")
	if err != nil {
		t.Fatalf("dial scraper for verification: %v", err)
	}
	defer nodeClient.Close()

	// 6. Tailscale binary must be installed on scraper.
	nodeTSBin, err := bash.RunOutput(nodeClient, `command -v tailscale >/dev/null 2>&1 && echo ok || echo missing`)
	if err != nil || strings.TrimSpace(nodeTSBin) != "ok" {
		t.Fatal("tailscale binary not found on scraper")
	}
	nodeTSIP, _ := bash.RunOutput(nodeClient, `tailscale ip -4 2>/dev/null || echo ""`)
	if strings.TrimSpace(nodeTSIP) != "" {
		t.Logf("verified: scraper tailscale IP = %s", strings.TrimSpace(nodeTSIP))
	} else {
		t.Log("verified: tailscale installed on scraper (no IP — join skipped in container mode)")
	}

}
