//go:build integration_multipass

package multipass

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	intssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// TestProvisioningSmoke is the smallest meaningful Multipass integration test:
//
//  1. load a two-machine manifest (gateway-host + node-host) from
//     testdata/manifest-provisioning.yml — the shape of a real headscale
//     deployment, but with OnlyPhases=[provisioning] so nothing past
//     create-machine/authorize-ssh-key/ensure-agent-user is exercised.
//  2. assert both VMs come up Running with routable IPv4s
//  3. assert ListByTag sees exactly the two VMs we just launched
//  4. tear them down via the same hosting.Provider
//
// Everything past provisioning (security, mesh, gateway, node, automations,
// agents) is intentionally out of scope — those get exercised by larger
// tests once this one is stable. The goal here is to prove the new
// `internal/hosting/multipass` provider + the generalized
// provisioning/security gates work on real hardware.
//
// Runtime on a laptop is ~30–60s depending on image cache state. A fresh
// `multipass launch 24.04` on cold caches can take several minutes while
// multipassd downloads the cloud image; the test budgets 5min per VM for
// that case.
func TestProvisioningSmoke(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-provisioning.yml")
	// Randomize the prefix at runtime so parallel test runs on the same
	// host don't collide on the generated VM labels (`<prefix>-<machine>`).
	// The YAML's default prefix is only used when loading outside the
	// harness (e.g. manual manifest lint); tests always override it.
	m.Prefix = "it-" + randSuffix(t)
	prefix := m.Prefix
	if len(m.Machines) < 1 {
		t.Fatalf("fixture sanity: expected at least one machine, got 0")
	}
	for i, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %d (%q) type = %q, want %q",
				i, mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}

	dial := sshDialFunc(signer)

	// Ensure any VM created by the plan (or left over from a previous
	// crashed run under this prefix) is cleaned up no matter how the test
	// exits. Running this BEFORE apply means a leaked VM from a prior
	// failed run gets nuked here too.
	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		insts, err := prov.ListByTag(cleanupCtx, "claws/"+prefix)
		if err != nil {
			t.Logf("cleanup: list instances: %v", err)
			return
		}
		for _, inst := range insts {
			t.Logf("cleanup: deleting %s", inst.Label)
			if err := prov.DeleteInstance(cleanupCtx, inst.ResourceID); err != nil {
				t.Logf("cleanup: delete %s: %v", inst.Label, err)
			}
		}
	})

	// --- plan ---------------------------------------------------------------

	plan, err := planapply.BuildPlan(planapply.BuildOptions{
		Manifest:  m,
		Provider:  prov,
		SSHPubKey: pubKey,
		SSHDial:   dial,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	// Sanity: provisioning must be a known phase in the plan we built.
	if !containsStr(plan.PhaseNames(), "provisioning") {
		t.Fatalf("plan is missing provisioning phase; got %v", plan.PhaseNames())
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	// Run ONLY provisioning. No pretty plan / confirm; this is a test, not
	// an operator session.
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- assertions ---------------------------------------------------------

	// Every machine must have an Instance with state=running and an IPv4.
	// Collect them once so we can assert on totals below (ListByTag /
	// destroy) without repeating the lookup.
	wantLabels := make(map[string]struct{}, len(m.Machines))
	for _, mc := range m.Machines {
		mt := findMachineTarget(t, plan, mc.Name)
		if mt.Instance == nil {
			t.Fatalf("machine %q has no Instance after apply", mc.Name)
		}
		inst := mt.Instance
		if inst.Status != "running" {
			t.Errorf("machine %q status = %q, want %q", mc.Name, inst.Status, "running")
		}
		if strings.TrimSpace(inst.PublicIPv4) == "" {
			t.Errorf("machine %q PublicIPv4 is empty; Instance: %+v", mc.Name, inst)
		} else if net.ParseIP(inst.PublicIPv4) == nil {
			t.Errorf("machine %q PublicIPv4 %q does not parse as an IP",
				mc.Name, inst.PublicIPv4)
		}
		wantLabels[inst.Label] = struct{}{}
		t.Logf("machine %q → label=%s ip=%s status=%s",
			mc.Name, inst.Label, inst.PublicIPv4, inst.Status)
	}

	// ListByTag must see exactly the VMs we just launched — this is the
	// path `claws destroy` uses, so exercising it here proves the sidecar
	// tag store was written during CreateInstance for every VM.
	insts, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after apply: %v", err)
	}
	if len(insts) != len(wantLabels) {
		t.Fatalf("ListByTag returned %d instances, want %d: %+v",
			len(insts), len(wantLabels), insts)
	}
	for _, inst := range insts {
		if _, ok := wantLabels[inst.Label]; !ok {
			t.Errorf("ListByTag returned unexpected label %q", inst.Label)
		}
	}

	// Actively destroy every instance here (belt-and-suspenders with
	// t.Cleanup) so DeleteInstance is exercised in the green path. If
	// this fails the Cleanup hook still runs and catches the leak.
	for _, inst := range insts {
		if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
			t.Fatalf("DeleteInstance %s: %v", inst.Label, err)
		}
	}
	after, err := prov.ListByTag(ctx, "claws/"+prefix)
	if err != nil {
		t.Fatalf("ListByTag after destroy: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("ListByTag after destroy returned %d instances, want 0: %+v", len(after), after)
	}
}

// ---------------------------------------------------------------------------
// helpers — kept in this file because the package only has one test today
// ---------------------------------------------------------------------------

// loadTestManifest reads a YAML manifest from testdata/ using the same
// parser the production CLI uses. That keeps the integration tier honest:
// if a field rename lands and the fixture silently stops being valid, this
// test fails at load rather than producing a confusing runtime error later.
func loadTestManifest(t *testing.T, name string) *manifestdata.Manifest {
	t.Helper()
	path := filepath.Join("testdata", name)
	m, err := manifestsvc.LoadFile(path)
	if err != nil {
		t.Fatalf("load manifest %s: %v", name, err)
	}
	return m
}

func generateEphemeralKey(t *testing.T) (privPath, pubKey string) {
	t.Helper()
	dir := t.TempDir()
	id, err := intssh.GeneratePEMIdentity(dir, "multipass-it")
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
	b, err := os.ReadFile(pub)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	return priv, strings.TrimSpace(string(b))
}

func loadSigner(t *testing.T, privPath string) xssh.Signer {
	t.Helper()
	data, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	s, err := xssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	return s
}

func sshDialFunc(signer xssh.Signer) provisioning.SSHDialFunc {
	return func(ctx context.Context, host string, port int, user string) (*xssh.Client, error) {
		cfg := &xssh.ClientConfig{
			User:            user,
			Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
			HostKeyCallback: xssh.InsecureIgnoreHostKey(),
			Timeout:         15 * time.Second,
		}
		d := net.Dialer{Timeout: 15 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		sshConn, chans, reqs, err := xssh.NewClientConn(conn, net.JoinHostPort(host, strconv.Itoa(port)), cfg)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return xssh.NewClient(sshConn, chans, reqs), nil
	}
}

func findMachineTarget(t *testing.T, plan *scaffold.Plan, name string) *provisioning.MachineTarget {
	t.Helper()
	for _, ph := range plan.Phases {
		for _, tgt := range ph.Targets {
			mt, ok := tgt.Payload.(*provisioning.MachineTarget)
			if !ok || mt == nil {
				continue
			}
			if mt.Spec.Name == name {
				return mt
			}
		}
	}
	t.Fatalf("machine target %q not found in plan", name)
	return nil
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// randSuffix returns 6 hex chars; enough entropy to avoid VM-name collisions
// between parallel test runs on the same host without needing a cleanup
// dance for repeated labels.
func randSuffix(t *testing.T) string {
	t.Helper()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// rewriteMeshHost rebuilds each gateway's custom public_hostname.host
// so it matches the hostname cloud-init actually pins inside the
// multipass VM — `<prefix>-<reference>.local` — after the test has
// randomized m.Prefix. Fixtures can't hardcode the final hostname
// because the prefix changes per run; this keeps the resolver side
// (manifest URL) and the provisioning side (cloud-init + multipass
// label) in lockstep without introducing template syntax into the
// manifest loader. Only custom-strategy .local hosts on port 8080 are
// rewritten — everything else (explicit FQDNs, sslip, non-mesh) is
// left untouched so the helper is safe to call unconditionally after
// loading any test manifest.
func rewriteMeshHost(m *manifestdata.Manifest) {
	for i := range m.Gateways {
		gw := &m.Gateways[i]
		if gw.Networking == nil || gw.Networking.PublicHostname == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(gw.Networking.PublicHostname.Strategy), "custom") {
			continue
		}
		if !strings.Contains(gw.Networking.PublicHostname.Host, ".local") {
			continue
		}
		ref := strings.TrimSpace(gw.Reference)
		if ref == "" {
			continue
		}
		gw.Networking.PublicHostname.Host = fmt.Sprintf("http://%s-%s.local:8080", m.Prefix, ref)
	}
}

