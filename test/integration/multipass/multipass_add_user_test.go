//go:build integration_multipass

package multipass

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshkeys"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
	intssh "github.com/gluwa/openclaw-swarm2/internal/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// TestSSHAddUser is the outside-in proof that the primitive `claws ssh add-user`
// is built on actually grants a second SSH identity access to a hardened VM.
//
// What this exercises that TestSecuritySmoke doesn't:
//
//   - The end-to-end grant flow: provision + harden with identity A, then
//     append identity B's public key over an A-authenticated SSH session,
//     then confirm B can dial in as agent_user where it previously couldn't.
//   - Negative control: B has zero SSH access before the append. If that
//     assertion fails the positive one is meaningless, so we check it first.
//   - Idempotency of the append: calling AppendAuthorizedKeyLinePOSIX twice
//     must leave exactly one copy of the line in authorized_keys. The append
//     script uses `grep -qxF` internally to short-circuit; this test is the
//     outside-in cross-check that the short-circuit actually fires on a
//     real VM with real sshd / bash / grep.
//
// What this deliberately does NOT exercise:
//
//   - The CLI flag parsing, prompting, state.Store, or cliutil.PrepareEndpoints
//     glue — those are covered by unit tests in
//     internal/cli/commands/remote/add_user_test.go.
//   - Multi-machine fan-out. The fixture is intentionally single-machine to
//     keep the runtime budget tight; per-target regressions are already
//     caught by TestSecuritySmoke running the same two-machine shape.
//
// Runtime budget: ~45s provisioning + ~60–120s security + a handful of
// seconds for the add-user dance. 12 minutes matches TestSecuritySmoke's
// per-test ceiling and absorbs a cold-apt worst case.
func TestSSHAddUser(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity A: the "primary" key that provisions + hardens the VM ---

	privPathA, pubKeyA := generateEphemeralKey(t)
	signerA := loadSigner(t, privPathA)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-add-user.yml")
	// Randomize the prefix so parallel runs don't step on each other's
	// VM labels. Same pattern as every other test in this tier.
	m.Prefix = "it-addu-" + randSuffix(t)
	prefix := m.Prefix
	if len(m.Machines) != 1 {
		t.Fatalf("fixture sanity: expected exactly one machine, got %d", len(m.Machines))
	}
	mc := m.Machines[0]
	if mc.Type != manifestdata.MachineTypeMultipass {
		t.Fatalf("fixture sanity: machine %q type = %q, want %q",
			mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
	}
	// Both a dedicated agent account AND a distinct bootstrap account are
	// the whole point — the CLI walks (agent_user, bootstrap_user) deduped,
	// and this test has to prove B gains access as both. If the fixture
	// ever drifts this test silently stops proving what it claims, so
	// fail loudly here.
	agentUser := strings.TrimSpace(mc.AgentUser)
	bootstrapUser := strings.TrimSpace(mc.BootstrapUser)
	if agentUser == "" {
		t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
	}
	if bootstrapUser == "" {
		t.Fatalf("fixture sanity: machine %q has empty bootstrap_user — "+
			"this test needs a distinct bootstrap account so add-user has "+
			"two users to deduplicate across", mc.Name)
	}
	if agentUser == bootstrapUser {
		t.Fatalf("fixture sanity: machine %q has agent_user == bootstrap_user "+
			"(%q) — this test needs them distinct to prove the CLI writes to "+
			"both authorized_keys files, not one", mc.Name, agentUser)
	}
	if agentUser == "ubuntu" {
		t.Fatalf("fixture sanity: machine %q agent_user is 'ubuntu' — "+
			"this test needs a dedicated agent account so ensure-agent-user "+
			"actually runs useradd and copies authorized_keys", mc.Name)
	}
	// targetUsers mirrors the CLI's ordering (agent first, bootstrap
	// second). We iterate the same slice for the negative control, the
	// as-A append dance, and the positive B-dial check below.
	targetUsers := []string{agentUser, bootstrapUser}

	// --- provider + SSH dialer ---------------------------------------------

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}

	dialA := sshDialFunc(signerA)

	// Register cleanup BEFORE apply so a mid-execution crash still nukes
	// partially-provisioned VMs. Copied from TestSecuritySmoke — this is
	// the only reliable way to avoid leaking VMs on a hard fail.
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
		SSHPubKey: pubKeyA,
		SSHDial:   dialA,
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	for _, want := range []string{"provisioning", "security"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}
	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress:   progress.Noop{},
		OnlyPhases: []string{"provisioning", "security"},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- resolve the live endpoint ----------------------------------------

	mt := findMachineTarget(t, plan, mc.Name)
	if mt.Instance == nil {
		t.Fatalf("machine %q has no Instance after apply", mc.Name)
	}
	host := strings.TrimSpace(mt.Instance.PublicIPv4)
	if host == "" || net.ParseIP(host) == nil {
		t.Fatalf("machine %q PublicIPv4 %q is not a routable IP", mc.Name, host)
	}
	port := 22
	if mc.SSHPort != 0 {
		port = mc.SSHPort
	}
	t.Logf("machine %q → targets %v @ %s:%d", mc.Name, targetUsers, host, port)

	// --- identity B: the "second laptop" key, independent of A -----------

	// Separate TempDir so we don't race intssh.GeneratePEMIdentity's
	// "directory already exists" guard against identity A's tempdir.
	dirB := t.TempDir()
	idB, err := intssh.GeneratePEMIdentity(dirB, "laptop-two")
	if err != nil {
		t.Fatalf("generate identity B: %v", err)
	}
	privPathB, err := intssh.ExpandPath(idB.PrivateKeyPath)
	if err != nil {
		t.Fatalf("expand B private key path: %v", err)
	}
	pubPathB, err := intssh.ExpandPath(idB.PublicKeyPath)
	if err != nil {
		t.Fatalf("expand B public key path: %v", err)
	}
	pubKeyBBytes, err := os.ReadFile(pubPathB)
	if err != nil {
		t.Fatalf("read B public key: %v", err)
	}
	pubKeyB := strings.TrimSpace(string(pubKeyBBytes))
	signerB := loadSigner(t, privPathB)
	dialB := sshDialFunc(signerB)

	// --- negative control: B cannot yet dial EITHER user -----------------

	// 10s ceiling: a successful auth handshake completes in well under a
	// second; a failing one returns in similar time. 10s is generous
	// enough to tolerate a transient TCP retry without masking a real
	// hang as a timeout-induced error that we'd then misclassify as an
	// auth failure. Both users are checked in sequence: the CLI will
	// write to both, so neither must already accept B, or the positive
	// assertion later would be meaningless.
	for _, u := range targetUsers {
		negCtx, negCancel := context.WithTimeout(ctx, 10*time.Second)
		if clientBad, err := dialB(negCtx, host, port, u); err == nil {
			clientBad.Close()
			negCancel()
			t.Fatalf("[%s] identity B dialed %s@%s before add-user — "+
				"provisioning must have silently authorized the wrong key",
				mc.Name, u, host)
		} else if !looksLikeAuthFailure(err) {
			// Non-auth errors (connection refused, context deadline, network
			// unreachable) would be false negatives for this control: they'd
			// pass the assertion for reasons unrelated to authorization.
			negCancel()
			t.Fatalf("[%s] identity B dial as %s failed in an unexpected way: %v",
				mc.Name, u, err)
		} else {
			t.Logf("[%s] negative control OK for %s: B auth rejected (%v)",
				mc.Name, u, err)
		}
		negCancel()
	}

	// --- append B's pubkey as A on BOTH users, then verify idempotency ---

	// The CLI's authorizeOnMachine dials (agent, bootstrap) and appends
	// to each. Outside-in, we exercise the same primitives once per
	// user: dial as A as that user, verify absent, append, verify
	// present, append again, then `grep -cxF` must return exactly 1.
	// Per-user append is isolated in its own closure so one user's
	// failure surfaces with a clear label and doesn't cross-contaminate
	// the other user's connection state.
	appendAsA := func(user string) {
		t.Helper()
		appendCtx, appendCancel := context.WithTimeout(ctx, 30*time.Second)
		defer appendCancel()
		clientA, err := dialA(appendCtx, host, port, user)
		if err != nil {
			t.Fatalf("[%s] dial %s@%s as identity A: %v",
				mc.Name, user, host, err)
		}
		defer clientA.Close()

		// Pre-condition: B's line is not yet in this user's
		// authorized_keys. Verify's bash script exits non-zero on miss,
		// so err != nil is the expected state here.
		if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(clientA, pubKeyB); err == nil {
			t.Fatalf("[%s/%s] VerifyAuthorizedKeyLinePOSIX already sees B's "+
				"line before we appended it — a previous run didn't clean up?",
				mc.Name, user)
		}

		// First append: must succeed and leave the line present.
		if err := sshkeys.AppendAuthorizedKeyLinePOSIX(clientA, pubKeyB); err != nil {
			t.Fatalf("[%s/%s] first AppendAuthorizedKeyLinePOSIX: %v",
				mc.Name, user, err)
		}
		if err := sshkeys.VerifyAuthorizedKeyLinePOSIX(clientA, pubKeyB); err != nil {
			t.Fatalf("[%s/%s] Verify after first append: %v",
				mc.Name, user, err)
		}

		// Second append: must succeed AND must not duplicate the line.
		// The append script uses `grep -qxF` to short-circuit when the
		// line already exists; the authoritative outside-in check is a
		// raw count via `grep -cxF` on the session user's own
		// authorized_keys (`~/.ssh/...` resolves to $HOME).
		if err := sshkeys.AppendAuthorizedKeyLinePOSIX(clientA, pubKeyB); err != nil {
			t.Fatalf("[%s/%s] second AppendAuthorizedKeyLinePOSIX: %v",
				mc.Name, user, err)
		}
		countCmd := fmt.Sprintf(
			"grep -cxF -- %s ~/.ssh/authorized_keys",
			shellSingleQuote(pubKeyB),
		)
		count, err := runSSHCommand(clientA, countCmd)
		if err != nil {
			t.Fatalf("[%s/%s] grep -cxF authorized_keys: %v",
				mc.Name, user, err)
		}
		if count != "1" {
			t.Errorf("[%s/%s] authorized_keys contains B's line %sx, want 1x",
				mc.Name, user, count)
		}
	}
	for _, u := range targetUsers {
		appendAsA(u)
	}

	// --- positive: B can now dial EACH target user and lands as that user

	for _, u := range targetUsers {
		posCtx, posCancel := context.WithTimeout(ctx, 20*time.Second)
		clientB, err := dialB(posCtx, host, port, u)
		if err != nil {
			posCancel()
			t.Fatalf("[%s] identity B dial %s@%s after add-user: %v",
				mc.Name, u, host, err)
		}
		whoami, err := runSSHCommand(clientB, "whoami")
		clientB.Close()
		posCancel()
		if err != nil {
			t.Fatalf("[%s] whoami as B@%s: %v", mc.Name, u, err)
		}
		if whoami != u {
			t.Errorf("[%s] whoami as B@%s = %q, want %q", mc.Name, u, whoami, u)
		}
		t.Logf("[%s] identity B logged in as %s (via %s account)",
			mc.Name, whoami, u)
	}
}

// ---------------------------------------------------------------------------
// helpers kept local to this test file
// ---------------------------------------------------------------------------

// runSSHCommand runs cmd over an existing SSH client and returns the
// trimmed combined output. A thinner twin of sshRunAsRoot (in
// multipass_security_test.go) that accepts any client — this test needs
// the same mechanics but with two different signers (A for the append,
// B for the positive whoami check), and pre-dialed clients rather than
// a dial-per-call.
func runSSHCommand(client *xssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// shellSingleQuote wraps s in POSIX single quotes, escaping any embedded
// single quotes with the `'\''` idiom so the string can be injected into
// a shell command without worrying about spaces or metacharacters. Used
// for the `grep -cxF -- '...'` idempotency check: a real OpenSSH pubkey
// line contains at least two spaces and a `+` / `/` / `=` from base64,
// all of which sh would otherwise mis-parse.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// looksLikeAuthFailure reports whether err's message looks like an SSH
// authentication rejection (as opposed to TCP refused / timeout /
// network-unreachable). We deliberately match on a handful of substrings
// rather than reach for a typed error: golang.org/x/crypto/ssh returns
// plain errors whose wording has shifted across minor versions, and
// pinning to one specific phrase made earlier tests brittle when the
// dep bumped.
func looksLikeAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unable to authenticate",
		"handshake failed",
		"permission denied",
		"no supported methods remain",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
