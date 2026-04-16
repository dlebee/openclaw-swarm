//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/service"
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
	t.Helper()
	path := filepath.Join("testdata", "manifest.yml")
	m, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
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
func ocContainer(t *testing.T, netName, pubKeyPath, name string) tc.Container {
	t.Helper()
	ctx := context.Background()
	t.Logf("start container: %s", name)
	absKey, err := filepath.Abs(pubKeyPath)
	if err != nil {
		t.Fatalf("abs pubkey path: %v", err)
	}
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        ocTestImage,
			ExposedPorts: []string{"22/tcp"},
			Networks:     []string{netName},
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

// TestApplyPlan is the main integration test:
//  1. Generates an ephemeral SSH identity ("integration")
//  2. Creates a shared Docker network
//  3. Starts gateway, scraper, and ollama containers with the pubkey injected
//  4. Verifies SSH connectivity using the ephemeral key
//  5. Builds the apply plan from the test manifest
//  6. Asserts every provisioning and security step is "not applicable"
//     for the ssh+container machines
func TestApplyPlan(t *testing.T) {
	privPath, pubPath := generateTestIdentity(t)
	t.Logf("identity: priv=%s pub=%s", privPath, pubPath)

	netName := testNetwork(t)

	gw := ocContainer(t, netName, pubPath, "gateway")
	scraper := ocContainer(t, netName, pubPath, "scraper")
	_ = ollamaContainer(t, netName)

	gwPort := mappedPort(t, gw, "22/tcp")
	scraperPort := mappedPort(t, scraper, "22/tcp")
	t.Logf("mapped ports: gateway=%d scraper=%d", gwPort, scraperPort)

	signer := sshSigner(t, privPath)
	waitSSH(t, "127.0.0.1", gwPort, "agent", signer, 30*time.Second)
	waitSSH(t, "127.0.0.1", scraperPort, "agent", signer, 30*time.Second)
	t.Log("SSH connectivity confirmed on gateway and scraper")

	// Load manifest and build the apply plan.
	m := loadTestManifest(t)

	// Patch manifest ports to match the dynamic mapped ports.
	for i := range m.Machines {
		switch m.Machines[i].Name {
		case "gateway-host":
			m.Machines[i].SSHPort = gwPort
		case "scraper-host":
			m.Machines[i].SSHPort = scraperPort
		}
	}

	plan, err := apply.BuildPlan(apply.BuildOptions{
		Manifest: m,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	ep, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	// Every step in provisioning and security must be "not applicable"
	// for ssh+container machines.
	ctx := context.Background()
	ctx = scaffold.EnsurePlanCache(ctx)

	for _, phase := range plan.Phases {
		for _, target := range phase.Targets {
			for _, step := range phase.Steps {
				ok, err := step.Applicable(ctx, target)
				if err != nil {
					t.Errorf("phase=%s target=%s step=%s: Applicable error: %v",
						phase.Name, target.ID, step.Name(), err)
					continue
				}
				if ok {
					t.Errorf("phase=%s target=%s step=%s: expected not applicable for ssh+container, got applicable",
						phase.Name, target.ID, step.Name())
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
