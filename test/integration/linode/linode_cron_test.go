//go:build integration_linode

package linode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	planapply "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting/linode"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Cron tuning knobs — mirror the Multipass tier's constants so a
// drift in one surfaces in both. Values match the docker tier's
// TestCronAgentWithNodeExec for identical scheduler coverage across
// Docker / Multipass / Linode.
//
// LLM backend: the reporter agent's "ollama" provider is pointed at
// a fake-ollama stub served by the gateway VM itself (not a real
// ollama install + qwen2.5:0.5b pull). See test/infra/fake-ollama.py
// for the stub and the package doc.go for the rationale. The fake
// still advertises the qwen2.5:0.5b model name so openclaw.json stays
// identical to the docker tier and so a regression in the agent's
// model-ref parsing surfaces uniformly across tiers.
const (
	linodeCronJobName        = "reporter-itest-linode"
	linodeCronAgentID        = "reporter"
	linodeCronTargetNodeName = "scraper-node"
	linodeCronInterval       = "5s"
	linodeCronWantRuns       = 2
	linodeCronRunsTimeout    = 5 * time.Minute
	linodeOllamaModel        = "qwen2.5:0.5b"
	linodeFakeOllamaPort     = 11499
	linodeFakeOllamaUnit     = "fake-ollama"
	linodeFakeOllamaRemote   = "/home/agent/fake-ollama.py"
)

type linodeCronJobJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type linodeCronRunEntry struct {
	Action     string `json:"action"`
	Status     string `json:"status"`
	RunAtMs    int64  `json:"runAtMs"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

type linodeCronRunsPage struct {
	Entries []linodeCronRunEntry `json:"entries"`
	Total   int                  `json:"total"`
}

// TestCronAgentWithNodeExec is the Linode counterpart to the
// Multipass tier's TestCronAgentWithNodeExec. It runs the full
// production-shape cron pipeline end-to-end on real Linode VMs:
// provisioning → security → mesh (with Caddy + Let's Encrypt ACME)
// → gateway → node → agents → fake-ollama start → cron job →
// scheduler fires → isolated agent turn → fake LLM returns exec
// tool call → exec dispatched over the tailnet ws to scraper-node
// → tool result returned → fake LLM returns final text → run
// persisted with status=ok.
//
// Why fake-ollama instead of real ollama + qwen2.5:0.5b: the
// scheduler / agent-session / exec-over-node coverage this tier
// cares about is independent of the LLM weights. A real ollama
// install costs ~400 MiB tarball download on amd64 + ~352 MiB model
// pull + ~15-30s cold-start on the first cron tick, with
// nondeterministic LLM output that forces slow timeouts and occasional
// flakes. The fake responds in <1 ms with a scripted exec tool call
// (turn 1) + scripted final text (turn 2), cutting the test from
// ~14 min → ~5 min and removing the only source of cron run duration
// variance. The docker tier still exercises real ollama (the
// oc-ollama-test image bakes qwen2.5:0.5b for free), so "real ollama
// plugin actually works" remains covered; this tier is about claws
// plumbing on a real cloud VM, which is exactly what fake-ollama
// leaves intact. See test/infra/fake-ollama.py for the stub and
// startFakeOllamaOnLinodeVM below for the boot sequence.
//
// Why this Linode mirror is worth the dollar-pennies cost on top
// of the Multipass tier's identical assertions:
//
//   - install-caddy + Let's Encrypt ACME under cron traffic. The
//     Multipass tier runs the mesh with plain http://, so the
//     Caddy/ACME path is untested on that tier. A regression where
//     the scheduler or the cron-triggered ts-node subprocess
//     somehow fails to reach the headscale control URL over HTTPS
//     surfaces here first.
//   - Cron traffic shape over real WireGuard between two Linode
//     VMs (gateway ↔ scraper-node). Multipass's same-host bridge
//     doesn't expose real MTU or timing characteristics; a
//     regression in how openclaw-node handles fragmented exec
//     dispatches or slow tailnet acks shows up here.
//   - `npm install -g openclaw` over real public egress. Same path
//     a customer deploy would use.
//
// Cost envelope: ~5-8 min on g6-standard-1 + g6-standard-1 at
// us-east ≈ 2 × $0.018/hr ≈ ~$0.005 per run. 15 min cap absorbs
// cold apt/npm caches and Let's Encrypt rate-limit backoff; there
// is no LLM cold-start contribution any more.
func TestCronAgentWithNodeExec(t *testing.T) {
	tok := loadLinodeToken(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ctx = scaffold.EnsurePlanCache(ctx)

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-cron.yml")
	m.Prefix = "it-lin-cron-" + randSuffix(t)
	prefix := m.Prefix

	if len(m.Machines) != 2 {
		t.Fatalf("fixture sanity: expected 2 machines, got %d", len(m.Machines))
	}
	if len(m.Gateways) != 1 {
		t.Fatalf("fixture sanity: expected 1 gateway, got %d", len(m.Gateways))
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("fixture sanity: expected 1 node, got %d", len(m.Nodes))
	}
	if len(m.Agents) != 1 {
		t.Fatalf("fixture sanity: expected 1 agent, got %d", len(m.Agents))
	}
	gw := &m.Gateways[0]
	if gw.Name != "gateway" {
		t.Fatalf("fixture sanity: expected gateway name \"gateway\", got %q", gw.Name)
	}
	// headscale + sslip — same production shape as TestNodeSmoke,
	// required so Caddy/ACME gets exercised. A drift to docker /
	// custom would silently weaken the cron coverage.
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale'; got %+v",
			gw.Name, gw.Networking)
	}
	if gw.Networking.PublicHostname == nil ||
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "sslip") {
		t.Fatalf("fixture sanity: gateway %q must set public_hostname.strategy=sslip (production default); got %+v",
			gw.Name, gw.Networking.PublicHostname)
	}

	nd := &m.Nodes[0]
	if nd.Name != linodeCronTargetNodeName {
		t.Fatalf("fixture sanity: expected node name %q, got %q", linodeCronTargetNodeName, nd.Name)
	}
	if nd.ExecPolicy == nil {
		t.Fatalf("fixture sanity: node %q must declare exec_policy (agent dispatches exec to it); got nil",
			nd.Name)
	}
	if !strings.EqualFold(nd.ExecPolicy.Security, "full") || !strings.EqualFold(nd.ExecPolicy.Ask, "off") {
		t.Fatalf("fixture sanity: node %q exec_policy must be {security:full, ask:off}; got %+v",
			nd.Name, nd.ExecPolicy)
	}

	ag := &m.Agents[0]
	if ag.ID != linodeCronAgentID {
		t.Fatalf("fixture sanity: expected agent id %q, got %q", linodeCronAgentID, ag.ID)
	}
	if ag.Tools == nil || ag.Tools.Exec == nil {
		t.Fatalf("fixture sanity: agent %q must declare tools.exec; got tools=%+v", ag.ID, ag.Tools)
	}
	if !strings.EqualFold(ag.Tools.Exec.Host, "node") {
		t.Fatalf("fixture sanity: agent %q tools.exec.host = %q, want \"node\"", ag.ID, ag.Tools.Exec.Host)
	}
	if ag.Tools.Exec.Node != linodeCronTargetNodeName {
		t.Fatalf("fixture sanity: agent %q tools.exec.node = %q, want %q",
			ag.ID, ag.Tools.Exec.Node, linodeCronTargetNodeName)
	}
	if !strings.HasPrefix(strings.ToLower(ag.Model.Primary), "ollama/") {
		t.Fatalf("fixture sanity: agent %q model.primary = %q, want prefix \"ollama/\"",
			ag.ID, ag.Model.Primary)
	}

	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeLinode {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeLinode)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
		}
		if mc.AgentUser == "root" {
			t.Fatalf("fixture sanity: machine %q agent_user must not be 'root' "+
				"(breaks systemd --user + linger coverage)", mc.Name)
		}
	}

	var gwMachine, nodeMachine manifestdata.Machine
	for _, mc := range m.Machines {
		switch mc.Name {
		case gw.Reference:
			gwMachine = mc
		case nd.Reference:
			nodeMachine = mc
		}
	}
	if gwMachine.Name == "" {
		t.Fatalf("fixture sanity: gateway reference %q matches no machine", gw.Reference)
	}
	if nodeMachine.Name == "" {
		t.Fatalf("fixture sanity: node reference %q matches no machine", nd.Reference)
	}

	// --- provider + SSH dialer ---------------------------------------------

	prov := linode.NewProvider(tok)
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node", "agents"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("milestone: applying full plan (mesh + gateway + node + agents) on 2 Linode VMs")
	if err := ex.Execute(ctx, scaffold.ExecuteOptions{
		Progress: progress.Noop{},
		OnlyPhases: []string{
			"provisioning", "security",
			"mesh-gateway", "mesh-join",
			"gateway", "node", "agents",
		},
	}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	// --- resolve public IPs post-apply -------------------------------------

	gwMT := findMachineTarget(t, plan, gwMachine.Name)
	if gwMT.Instance == nil {
		t.Fatalf("gateway machine %q has no Instance after apply", gwMachine.Name)
	}
	gwInst := gwMT.Instance
	if gwInst.Status != "running" {
		t.Errorf("gateway machine %q status = %q, want %q", gwMachine.Name, gwInst.Status, "running")
	}
	if strings.TrimSpace(gwInst.PublicIPv4) == "" || net.ParseIP(gwInst.PublicIPv4) == nil {
		t.Fatalf("gateway machine %q PublicIPv4 %q is not a valid IP", gwMachine.Name, gwInst.PublicIPv4)
	}
	gwPublicIP := gwInst.PublicIPv4
	t.Logf("gateway VM → %s (public=%s)", gwInst.Label, gwPublicIP)

	nodeMT := findMachineTarget(t, plan, nodeMachine.Name)
	if nodeMT.Instance == nil {
		t.Fatalf("node machine %q has no Instance after apply", nodeMachine.Name)
	}
	nodeInst := nodeMT.Instance
	if nodeInst.Status != "running" {
		t.Errorf("node machine %q status = %q, want %q", nodeMachine.Name, nodeInst.Status, "running")
	}
	nodePublicIP := nodeInst.PublicIPv4
	t.Logf("node VM → %s (public=%s)", nodeInst.Label, nodePublicIP)

	// --- Sanity: gateway up, node paired -----------------------------------

	assertGatewayUnitActive(t, dial, gwPublicIP, gwMachine)
	assertGatewayPortListeningOnLAN(t, dial, gwPublicIP, gwMachine)
	assertNodeUnitActive(t, dial, nodePublicIP, nodeMachine)
	assertNodePairedOnGateway(t, dial, gwPublicIP, gwMachine, nd.Name)

	// --- Fake Ollama stub on the gateway VM --------------------------------
	//
	// SFTP-upload test/infra/fake-ollama.py and start it under
	// systemd --user so it survives SSH disconnects and gets
	// cleaned up automatically when the VM is deleted. See
	// startFakeOllamaOnLinodeVM for the boot + readiness sequence
	// and the package doc.go for why we use a stub here instead
	// of installing real ollama.

	t.Log("milestone: uploading + starting fake-ollama on gateway VM")
	startFakeOllamaOnLinodeVM(t, dial, gwPublicIP, gwMachine)

	t.Log("milestone: sanity-checking fake-ollama on loopback")
	verifyFakeOllamaReachable(t, dial, gwPublicIP, gwMachine)

	t.Log("milestone: configuring models.providers.ollama.baseUrl")
	configureLinodeOllamaProvider(t, dial, gwPublicIP, gwMachine)

	// --- Cron job lifecycle -------------------------------------------------

	t.Log("milestone: reading gateway auth token")
	gatewayToken := readGatewayTokenOverSSH(t, dial, gwPublicIP, gwMachine)
	t.Logf("gateway token present (len=%d)", len(gatewayToken))

	t.Log("milestone: creating cron job (every 5s, reporter agent)")
	jobID := createLinodeCronJob(t, dial, gwPublicIP, gwMachine, gatewayToken)
	t.Logf("cron job created: id=%q", jobID)
	t.Cleanup(func() {
		removeLinodeCronJob(t, dial, gwPublicIP, gwMachine, gatewayToken, jobID)
	})

	t.Logf("milestone: waiting for %d cron runs (timeout %s)",
		linodeCronWantRuns, linodeCronRunsTimeout)
	runs := waitForLinodeCronRuns(t, dial, gwPublicIP, gwMachine,
		gatewayToken, jobID, linodeCronWantRuns, linodeCronRunsTimeout)
	t.Logf("milestone: got %d cron runs", len(runs))

	var failed int
	for i, r := range runs {
		if r.Status == "ok" {
			t.Logf("run %d: ok (duration=%dms) summary=%q",
				i+1, r.DurationMs, linodeTruncate(r.Summary, 80))
			continue
		}
		failed++
		t.Errorf("run %d: expected status=ok, got %q (error=%q summary=%q)",
			i+1, r.Status, r.Error, r.Summary)
	}
	if failed > 0 {
		t.Fatalf("%d/%d cron runs failed", failed, len(runs))
	}

	t.Log("milestone: TestCronAgentWithNodeExec PASSED")
}

// ---------------------------------------------------------------------------
// Fake ollama helpers
// ---------------------------------------------------------------------------

// startFakeOllamaOnLinodeVM uploads test/infra/fake-ollama.py and
// starts it as a systemd --user service on the gateway. We use
// systemd --user (not nohup + disown) because:
//
//   - The security phase runs `loginctl enable-linger agent` so
//     user units survive SSH disconnects unconditionally; this is
//     the same mechanism the gateway/node's own openclaw-gateway
//     and openclaw-node units rely on, so reusing it here means
//     "if fake-ollama doesn't survive, neither would the gateway"
//     — which would have surfaced long before cron.
//   - systemd-run cleanly captures logs (`journalctl --user -u
//     fake-ollama`) which the test can pull if readiness or a
//     cron run fails.
//   - The unit auto-stops on shutdown when Linode tears the VM
//     down, so there's no "dangling background process" cleanup.
//
// The script is uploaded via SFTP (sshfile.WriteFile) rather than
// a heredoc over SSH so non-trivial indentation + triple-quoted
// Python strings survive the round trip intact.
func startFakeOllamaOnLinodeVM(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()

	scriptPath := findFakeOllamaScript(t)
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read fake-ollama.py: %v", err)
	}

	dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	client, err := dial(dctx, host, 22, user)
	if err != nil {
		t.Fatalf("[%s] dial for fake-ollama upload: %v", mc.Name, err)
	}
	defer client.Close()

	if err := sshfile.WriteFile(client, linodeFakeOllamaRemote, data); err != nil {
		t.Fatalf("[%s] upload fake-ollama.py to %s: %v", mc.Name, linodeFakeOllamaRemote, err)
	}
	t.Logf("[%s] fake-ollama.py uploaded (%d bytes) to %s", mc.Name, len(data), linodeFakeOllamaRemote)

	// Stop any stale instance (safe no-op on first boot) + start
	// fresh. --property=Type=simple keeps systemd happy with a
	// script that blocks in serve_forever(). --setenv pins the
	// port so a `FAKE_OLLAMA_PORT` drift in the python defaults
	// wouldn't break the test.
	startScript := fmt.Sprintf(`set -eux
chmod +x %[2]s
systemctl --user stop %[1]s 2>/dev/null || true
systemctl --user reset-failed %[1]s 2>/dev/null || true
systemd-run --user \
    --unit=%[1]s \
    --property=Type=simple \
    --setenv=FAKE_OLLAMA_PORT=%[3]d \
    /usr/bin/python3 %[2]s
`, linodeFakeOllamaUnit, linodeFakeOllamaRemote, linodeFakeOllamaPort)

	out, err := sshRunAsGatewayAgent(t, dial, host, mc, startScript)
	if err != nil {
		t.Fatalf("[%s] systemd-run fake-ollama: %v\n%s", mc.Name, err, out)
	}

	// Wait for the stub to answer /api/tags on loopback. The
	// systemd transient unit is "active" almost immediately but
	// Python's http.server needs a few hundred ms to bind the
	// socket; 30s is ample headroom for slow disk-caches.
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := sshRunAsGatewayAgent(t, dial, host, mc,
			fmt.Sprintf(`curl -sS --max-time 3 http://127.0.0.1:%d/api/tags 2>&1 || echo "(curl failed)"`, linodeFakeOllamaPort))
		if err == nil && strings.Contains(out, linodeOllamaModel) {
			t.Logf("[%s] fake-ollama ready on :%d: %s", mc.Name, linodeFakeOllamaPort, linodeTruncate(out, 120))
			return
		}
		last = out
		time.Sleep(1 * time.Second)
	}

	// Pull journal tail so the failure log says *why* the stub
	// didn't come up (stdlib import error, port in use, etc.).
	journal, _ := sshRunAsGatewayAgent(t, dial, host, mc,
		fmt.Sprintf(`journalctl --user -u %s --no-pager -n 40 2>&1 || true`, linodeFakeOllamaUnit))
	t.Fatalf("[%s] fake-ollama never answered on :%d (last curl: %s)\n--- journal ---\n%s",
		mc.Name, linodeFakeOllamaPort, last, journal)
}

// verifyFakeOllamaReachable sanity-pings /api/chat on the fake to
// prove the stub is alive + returns the expected tool-call payload
// before we hand off to the cron scheduler. A failure here pins the
// regression to fake-ollama itself rather than to the openclaw/ollama
// plugin or the cron scheduler.
func verifyFakeOllamaReachable(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	payload := fmt.Sprintf(
		`curl -sS --max-time 5 http://127.0.0.1:%d/api/chat `+
			`-H "Content-Type: application/json" `+
			`-d '{"messages":[{"role":"user","content":"probe"}]}'`,
		linodeFakeOllamaPort,
	)
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, payload)
	if err != nil {
		t.Fatalf("[%s] fake-ollama /api/chat probe: %v\n%s", mc.Name, err, out)
	}
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"exec"`) {
		t.Fatalf("[%s] fake-ollama didn't return an exec tool call:\n%s", mc.Name, out)
	}
	t.Logf("[%s] fake-ollama ok: %s", mc.Name, linodeTruncate(out, 120))
}

// configureLinodeOllamaProvider points the gateway's Ollama provider
// at the fake-ollama stub on 127.0.0.1:<fake port>. We use the
// hostname "localhost" (not "127.0.0.1") so the ollama plugin's
// hasMeaningfulExplicitOllamaConfig() check treats the baseUrl as
// non-default and mints a synthetic local auth token — without that,
// isolated cron-agent sessions fail with "No API key found for
// provider 'ollama'". The plugin's check is a literal string compare
// against OLLAMA_DEFAULT_BASE_URL ("http://127.0.0.1:11434") so any
// string-different-but-routing-equivalent URL works; "localhost" still
// resolves to 127.0.0.1 at dial time. The non-11434 port also means
// this config can never be mistaken for "point at a real ollama
// install"; if a future refactor accidentally drops the fake, cron
// runs fail loudly with a connection-refused instead of silently
// hitting a stale real-ollama daemon.
func configureLinodeOllamaProvider(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	script := fmt.Sprintf(`set -e
node -e '
const fs = require("fs"), os = require("os");
const p = os.homedir() + "/.openclaw/openclaw.json";
const c = JSON.parse(fs.readFileSync(p, "utf8"));
if (!c.models) c.models = {};
if (!c.models.providers) c.models.providers = {};
c.models.providers.ollama = { baseUrl: "http://localhost:%d", models: [] };
fs.writeFileSync(p, JSON.stringify(c, null, 2) + "\n");
'
`, linodeFakeOllamaPort)
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, script)
	if err != nil {
		t.Fatalf("[%s] configure ollama provider: %v\n%s", mc.Name, err, out)
	}
}

// findFakeOllamaScript resolves test/infra/fake-ollama.py relative
// to the running test binary's source tree. Tests run with go test
// under the package directory; we walk up until we find the repo
// root marker "go.mod" and then join the known relative path. This
// avoids baking an absolute path into the fixture and lets the test
// work from any checkout location.
func findFakeOllamaScript(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			p := filepath.Join(dir, "test", "infra", "fake-ollama.py")
			if _, err := os.Stat(p); err == nil {
				return p
			}
			t.Fatalf("fake-ollama.py not at %s", p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find repo root (go.mod) walking up from %s", wd)
	return ""
}

// ---------------------------------------------------------------------------
// Gateway token + cron RPC helpers
// ---------------------------------------------------------------------------

func readGatewayTokenOverSSH(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) string {
	t.Helper()
	ctxDial, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	client, err := dial(ctxDial, host, 22, user)
	if err != nil {
		t.Fatalf("[%s] dial for token read: %v", mc.Name, err)
	}
	defer client.Close()
	home, err := gwService.ResolveHome(client)
	if err != nil {
		t.Fatalf("[%s] resolve home: %v", mc.Name, err)
	}
	token, err := gwService.ReadToken(client, home)
	if err != nil || token == "" {
		t.Fatalf("[%s] read gateway token: err=%v tokenLen=%d", mc.Name, err, len(token))
	}
	return token
}

func createLinodeCronJob(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken string) string {
	t.Helper()
	prompt := "Tell me a random one-sentence fact. " +
		"Reply with exactly one short English sentence, nothing else."
	script := fmt.Sprintf(
		`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron add `+
			`--name %q `+
			`--agent %q `+
			`--message %q `+
			`--every %q `+
			`--session isolated `+
			`--no-deliver`,
		gatewayToken, linodeCronJobName, linodeCronAgentID, prompt, linodeCronInterval,
	)
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, script)
	if err != nil {
		t.Fatalf("[%s] cron add: %v\n%s", mc.Name, err, out)
	}
	raw := extractFirstJSON(out)
	if raw == "" {
		t.Fatalf("[%s] cron add: no JSON in output:\n%s", mc.Name, out)
	}
	var job linodeCronJobJSON
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("[%s] parse cron add output: %v\nraw=%s", mc.Name, err, raw)
	}
	if job.ID == "" {
		t.Fatalf("[%s] cron add: empty job id in %s", mc.Name, raw)
	}
	return job.ID
}

func removeLinodeCronJob(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string) {
	t.Helper()
	script := fmt.Sprintf(`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron rm %q 2>/dev/null || true`, gatewayToken, jobID)
	_, _ = sshRunAsGatewayAgent(t, dial, host, mc, script)
	t.Logf("cleanup: cron job %q removed", jobID)
}

func waitForLinodeCronRuns(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string, wantRuns int, timeout time.Duration) []linodeCronRunEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("[%s] timed out waiting for %d cron runs (job %q)", mc.Name, wantRuns, jobID)
		}
		entries := fetchLinodeCronRuns(t, dial, host, mc, gatewayToken, jobID)
		finished := filterLinodeFinished(entries)
		t.Logf("[%s] cron runs: %d finished of %d wanted (raw entries: %d)",
			mc.Name, len(finished), wantRuns, len(entries))
		if len(finished) >= wantRuns {
			return finished[:wantRuns]
		}
		time.Sleep(3 * time.Second)
	}
}

func fetchLinodeCronRuns(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string) []linodeCronRunEntry {
	t.Helper()
	script := fmt.Sprintf(`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron runs --id %q --limit 20`, gatewayToken, jobID)
	out, err := sshRunAsGatewayAgentLong(t, dial, host, mc, script, 60*time.Second)
	if err != nil {
		t.Logf("[%s] cron runs (non-fatal): %v\n%s", mc.Name, err, out)
		return nil
	}
	raw := extractFirstJSON(out)
	if raw == "" {
		return nil
	}
	var page linodeCronRunsPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Logf("[%s] parse cron runs: %v\nraw=%s", mc.Name, err, raw)
		return nil
	}
	return page.Entries
}

func filterLinodeFinished(entries []linodeCronRunEntry) []linodeCronRunEntry {
	out := make([]linodeCronRunEntry, 0, len(entries))
	for _, e := range entries {
		if e.Action == "finished" && e.Status != "" {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Small utilities kept local to avoid cross-tier coupling
// ---------------------------------------------------------------------------

// sshRunAsGatewayAgentLong is a long-timeout variant of
// sshRunAsGatewayAgent. Needed for ollama install / pull / generate
// which each legitimately exceed the short-lived 45s envelope the
// tier's sshRunAsGatewayAgent imposes. See the multipass tier's
// sshRunAsGatewayAgentLong for the full rationale; this is the
// Linode adaptation with no behavioral differences.
func sshRunAsGatewayAgentLong(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, cmd string, timeout time.Duration) (string, error) {
	t.Helper()
	dctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	user := mc.AgentUser
	if user == "" {
		user = "root"
	}
	client, err := dial(dctx, host, 22, user)
	if err != nil {
		return "", fmt.Errorf("dial %s@%s: %w", user, host, err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

func extractFirstJSON(raw string) string {
	start := strings.Index(raw, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1]
			}
		}
	}
	return ""
}

func linodeTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
