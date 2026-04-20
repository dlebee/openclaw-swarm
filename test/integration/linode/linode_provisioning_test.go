//go:build integration_linode

package linode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	intssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// TestProvisioningSmoke is the smallest meaningful Linode integration
// test: it mirrors the Multipass tier's TestProvisioningSmoke but
// exercises the linode hosting.Provider against real API hardware.
//
//  1. load a two-machine manifest (gateway-host + node-host) from
//     testdata/manifest-provisioning.yml — same shape as a real
//     headscale deployment, but with OnlyPhases=[provisioning] so
//     nothing past create-machine / authorize-ssh-key /
//     ensure-agent-user is exercised.
//  2. assert both Linode instances come up Running with public IPv4s
//  3. assert ListByTag sees exactly the two instances we just launched
//     under the claws/<prefix> tag
//  4. tear them down via DeleteInstance so the destroy code path is
//     covered and no leaks are charged
//
// Runtime budget: Linode API + provisioning is dominated by
// `provisioner` status (~45–90s per instance in parallel) plus SSH
// availability (another ~15s after boot). 10 minutes is a comfortable
// cap for the whole flight.
func TestProvisioningSmoke(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
		data, err := os.ReadFile(privPath)
		if err == nil {
			saved := "/tmp/linode-provisioning-it-key"
			_ = os.WriteFile(saved, data, 0o600)
			t.Logf("CLAWS_IT_KEEP_VMS: private key saved to %s (use: ssh -i %s root@<ip>)", saved, saved)
		}
	}

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-provisioning.yml")
	// Randomize prefix so concurrent runs on the same account don't
	// collide on Linode labels (prefix-<machine>). The YAML's default
	// is only used when loading outside the harness.
	m.Prefix = "it-lin-" + randSuffix(t)
	prefix := m.Prefix
	if len(m.Machines) < 1 {
		t.Fatalf("fixture sanity: expected at least one machine, got 0")
	}
	for i, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %d (%q) type = %q, want %q",
				i, mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
	}

	// --- provider + SSH dialer ---------------------------------------------

	// Bypass ProviderFromManifest — the fixture deliberately doesn't
	// set linode_token_env/env_file so it stays self-contained. The
	// harness loaded the token out-of-band (loadLinodeToken), we pass
	// it straight to the client here.
	prov := linode.NewProvider(tok)

	dial := sshDialFunc(signer)

	// Register cleanup BEFORE apply so a crash mid-execution still
	// destroys whatever was created. Running ListByTag + DeleteInstance
	// here under a fresh context also nukes leftovers from a previous
	// crashed run that happened to pick the same prefix (unlikely given
	// the random suffix, but cheap insurance).
	t.Cleanup(func() {
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		insts, err := prov.ListByTag(cleanupCtx, "claws/"+prefix)
		if err != nil {
			t.Logf("cleanup: list instances: %v", err)
			return
		}
		for _, inst := range insts {
			t.Logf("cleanup: deleting %s (%s)", inst.Label, inst.ResourceID)
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

	if !containsStr(plan.PhaseNames(), "provisioning") {
		t.Fatalf("plan is missing provisioning phase; got %v", plan.PhaseNames())
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- assertions ---------------------------------------------------------

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

	if os.Getenv("CLAWS_IT_KEEP_VMS") == "" {
		for _, inst := range insts {
			if err := prov.DeleteInstance(ctx, inst.ResourceID); err != nil {
				t.Fatalf("DeleteInstance %s: %v", inst.Label, err)
			}
		}
		// Linode deletes are async — give the API a few seconds to reflect
		// the deletion before we re-list. In practice 5s is enough; 30s
		// total retry budget is paranoid insurance against a slow account.
		after := waitForEmptyListByTag(ctx, t, prov, "claws/"+prefix, 30*time.Second)
		if len(after) != 0 {
			t.Errorf("ListByTag after destroy still returned %d instances: %+v",
				len(after), after)
		}
	} else {
		t.Logf("CLAWS_IT_KEEP_VMS: skipping active destroy after green run (prefix=%s); t.Cleanup also skipped", prefix)
	}
}

// ---------------------------------------------------------------------------
// helpers — shared across the three tests in this package
// ---------------------------------------------------------------------------

// loadLinodeToken returns the API token used by every test in this
// package. The two-source fallback mirrors how the CLI itself resolves
// env vars in LookupEnvFromManifest: process env wins, `.env` file is
// the documented fallback. We stop at this layer (rather than calling
// LookupEnvFromManifest directly) because these tests deliberately
// don't encode env_file/linode_token_env in their fixtures — that
// would require YAML paths that reach outside the test package's
// directory tree and break `go test -run` usage in arbitrary CWDs.
//
// If neither source yields a token, the test skips (not fails): the
// Linode integration tier is opt-in by design, same way
// TestProvisioningSmoke in the Multipass tier skips when the multipass
// CLI isn't installed.
func loadLinodeToken(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("LINODE_TOKEN")); v != "" {
		return v
	}
	// Fallback: read the repo-local shared .env. The path is fixed
	// because the whole integration tier lives inside this repo; when
	// someone drops it into another monorepo the env var path will
	// kick in instead.
	rel := filepath.Join("..", "..", "..", "..", "manifests", ".env")
	abs, err := filepath.Abs(rel)
	if err == nil {
		if vars, rerr := godotenv.Read(abs); rerr == nil {
			if v := strings.TrimSpace(vars["LINODE_TOKEN"]); v != "" {
				t.Logf("using LINODE_TOKEN from %s", abs)
				return v
			}
		}
	}
	t.Skipf("LINODE_TOKEN not set in environment and not found in %s; skipping Linode integration test", rel)
	return ""
}

// loadTestManifest reads a YAML manifest from testdata/ using the same
// parser the production CLI uses. Keeping the integration tier honest:
// if a field rename lands and the fixture silently stops being valid,
// this test fails at load rather than producing a confusing runtime
// error later.
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
	id, err := intssh.GeneratePEMIdentity(dir, "linode-it")
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
			Timeout:         20 * time.Second,
		}
		d := net.Dialer{Timeout: 20 * time.Second}
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

// randSuffix returns 6 hex chars; enough entropy to avoid Linode-label
// collisions between parallel runs on the same account.
func randSuffix(t *testing.T) string {
	t.Helper()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// waitForEmptyListByTag polls ListByTag until it returns zero
// instances or the budget expires. Linode deletes are async: the
// DELETE returns 200 but the instance can still appear in
// GET /linode/instances for a few seconds while the hypervisor
// unwinds disks + network. Returning the last non-empty slice lets
// the caller log it if we gave up.
//
// The 3s poll cadence is a compromise: fast enough that green-path
// teardowns don't hang, slow enough that we don't hammer the API
// (which enforces rate limits per account).
func waitForEmptyListByTag(ctx context.Context, t *testing.T, prov *linode.Provider, tag string, budget time.Duration) []hosting.Instance {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []hosting.Instance
	for time.Now().Before(deadline) {
		insts, err := prov.ListByTag(ctx, tag)
		if err != nil {
			t.Logf("waitForEmptyListByTag: %v", err)
			return last
		}
		if len(insts) == 0 {
			return nil
		}
		last = insts
		select {
		case <-ctx.Done():
			return last
		case <-time.After(3 * time.Second):
		}
	}
	return last
}
