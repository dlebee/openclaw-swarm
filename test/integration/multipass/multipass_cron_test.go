//go:build integration_multipass

package multipass

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
	"github.com/gluwa/openclaw-swarm2/internal/hosting/multipass"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/sshfile"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold/progress"
)

// Cron-specific tuning knobs. The values mirror the docker tier's
// TestCronAgentWithNodeExec (test/integration/docker/cron_test.go) so
// a regression that changes the cron run shape in one place also
// surfaces here — keeping the signal/noise identical across tiers is
// the whole point of mirroring the knobs.
//
// LLM backend: the reporter agent's "ollama" provider is pointed at
// a fake-ollama stub served by the gateway VM itself (not a real
// ollama install + qwen2.5:0.5b pull). See test/infra/fake-ollama.py
// for the stub and the package doc.go for the rationale. The fake
// still advertises the qwen2.5:0.5b model name so openclaw.json stays
// identical to the docker tier and so a regression in the agent's
// model-ref parsing surfaces uniformly across tiers.
const (
	multipassCronJobName        = "reporter-itest-multipass"
	multipassCronAgentID        = "reporter"
	multipassCronTargetNodeName = "scraper-node"
	multipassCronInterval       = "5s"
	// Two successful runs is the minimum that still proves the
	// scheduler can fire the same job twice (catches "first-run
	// worked, persistence broke for run N+1" regressions) while
	// keeping the test's runtime-per-run envelope bounded. With
	// fake-ollama the per-run LLM call is <1ms so the timeout is
	// dominated by ts-node subprocess spawn (~2s) and cron tick
	// interval (5s) — a 3 min cap leaves ample headroom.
	multipassCronWantRuns    = 2
	multipassCronRunsTimeout = 3 * time.Minute
	// Fake-ollama tuning knobs. The port deliberately diverges from
	// Ollama's canonical 11434 so the config can never be mistaken
	// for "point at a real ollama install" — a drop of the fake
	// would surface as connection-refused rather than silently
	// hitting a stale daemon.
	multipassOllamaModel     = "qwen2.5:0.5b"
	multipassFakeOllamaPort  = 11499
	multipassFakeOllamaUnit  = "fake-ollama"
	multipassFakeOllamaRemot = "/home/agent/fake-ollama.py"
)

type multipassCronJobJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type multipassCronRunEntry struct {
	Action     string `json:"action"`
	Status     string `json:"status"`
	RunAtMs    int64  `json:"runAtMs"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

type multipassCronRunsPage struct {
	Entries []multipassCronRunEntry `json:"entries"`
	Total   int                     `json:"total"`
}

// TestCronAgentWithNodeExec is the ninth Multipass integration test
// (after provisioning, security, mesh, gateway, channels, node,
// agents, and their mesh-over-gateway composite). It exercises the
// full end-to-end cron pipeline on real VMs over a real tailnet:
// scheduler → isolated agent turn → LLM call (fake-ollama stub on
// the gateway VM) → tool-call dispatched over the node websocket to
// a remote node (scraper-node) → tool result returned → final LLM
// text → run persisted by the scheduler with status=ok.
//
// What this exercises that NONE of the earlier tests do:
//
//   - The cron scheduler subsystem. Every earlier apply-phase test
//     stops at "the daemon config landed" — this test is the first
//     to prove openclaw's scheduler actually fires a registered job,
//     and fires it repeatedly. A regression in cron-scheduler state
//     persistence (e.g. a run-log mutex deadlock or an isolated-
//     session queue starvation bug) would show here as "first run
//     ok, all subsequent runs stuck".
//   - Isolated-session ts-node subprocess spawning. Each cron tick
//     cold-boots a fresh ts-node agent process (distinct from the
//     long-lived gateway daemon). Regressions in process-spawn
//     plumbing, env propagation, or child-process stdout handling
//     surface on the very first run.
//   - End-to-end agent → ollama-plugin → LLM-endpoint plumbing. The
//     ollama plugin auto-discovers models via /api/tags and
//     synthesizes a local auth token whenever baseUrl differs from
//     the default. Fake-ollama answers /api/tags and /api/chat with
//     canned payloads so a regression in either path manifests here
//     as status=error with a concrete error string in the run log.
//   - Exec-tool dispatch over the tailnet. The reporter agent's
//     tools.exec pins every exec tool-call to scraper-node; every
//     tool-call fake-ollama emits rides over the tailnet ws from
//     the gateway to the scraper VM. This is the single path that
//     proves mesh + gateway + node + agents all cooperate under
//     real traffic (not just "config landed and the ws handshake
//     opened"). TestNodeSmoke proves the ws opens; TestAgentsSmoke
//     proves the agent config writes; this test is the FIRST to
//     prove the ws actually carries production traffic.
//   - Node-side exec_policy enforcement (security: full + ask: off).
//     TestNodeSmoke deliberately omits exec_policy to cover the
//     Applicable=false branch; this test flips to the Applicable=
//     true branch and proves the node's exec handler accepts
//     dispatches when the policy allows them.
//
// Why fake-ollama instead of real ollama + qwen2.5:0.5b:
//
//   There is no apply phase that installs Ollama — claws applies
//   config to the gateway/node but the LLM backend is whatever the
//   user points models.providers.ollama.baseUrl at. The earlier
//   iteration of this test installed real ollama via the vendor
//   script and pulled qwen2.5:0.5b. On Apple Silicon Multipass that
//   tarball is 1.3 GiB for linux-arm64; on a slow home ISP the
//   download alone dominated the test (~50 min), and LLM generate
//   added another ~30-90s per cron run with nondeterministic output
//   that forced loose assertions.
//
//   The switch to test/infra/fake-ollama.py — a ~150-line stdlib
//   Python HTTP server that impersonates the minimum /api/* surface
//   the openclaw ollama plugin hits — removes both costs. The stub
//   answers in <1 ms with a scripted exec tool-call (turn 1) +
//   scripted final text (turn 2), cutting the test from ~60-90 min
//   to ~5 min and giving deterministic cron-run summaries. The
//   docker tier's TestCronAgentWithNodeExec still exercises real
//   ollama against a pre-baked oc-ollama-test image, so "real
//   ollama plugin works" remains covered; this tier is about
//   "claws + systemd + tailnet survive on a real VM", which is
//   what fake-ollama leaves intact.
//
// Phase frontier: provisioning, security, mesh-gateway, mesh-join,
// gateway, node, agents. No channels (cron add --no-deliver skips
// the announce delivery path; channels integration lives in
// TestChannelsSmoke and is a separate concern). mesh.AddPhase
// splits mesh into two sub-phases when any gateway is headscale.
//
// Runtime budget: provisioning ~60s × 2 + security ~35s × 2 + mesh-
// gateway ~60s + mesh-join ~30s + gateway ~3 min + node ~2 min +
// agents ~20s + fake-ollama upload+start ~5s + 2 cron runs ~20s.
// ~15 min test cap covers worst-case apt/npm cold caches.
func TestCronAgentWithNodeExec(t *testing.T) {
	if !multipass.IsBinaryAvailable() {
		t.Skip("multipass not on PATH (install from https://multipass.run)")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// --- identity -----------------------------------------------------------

	privPath, pubKey := generateEphemeralKey(t)
	signer := loadSigner(t, privPath)

	// --- manifest -----------------------------------------------------------

	m := loadTestManifest(t, "manifest-cron.yml")
	m.Prefix = "it-cron-" + randSuffix(t)
	// See rewriteMeshHost — realign the fixture's custom control URL
	// with the `<prefix>-gateway-host` hostname cloud-init will pin.
	rewriteMeshHost(m)
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
	// headscale + mDNS control URL — same mesh shape as TestNodeSmoke.
	// If this regresses to mode:docker the cron test would still pass
	// on a Docker bridge but stop being a production-shape test.
	if gw.Networking == nil || !strings.EqualFold(gw.Networking.Mode, "headscale") {
		t.Fatalf("fixture sanity: gateway %q networking.mode must be 'headscale' (cron over mesh is the production path); got %+v",
			gw.Name, gw.Networking)
	}
	if gw.Networking.PublicHostname == nil ||
		!strings.EqualFold(gw.Networking.PublicHostname.Strategy, "custom") ||
		!strings.Contains(gw.Networking.PublicHostname.Host, ".local") {
		t.Fatalf("fixture sanity: gateway %q must set public_hostname.strategy=custom with a .local host, got %+v",
			gw.Name, gw.Networking.PublicHostname)
	}

	nd := &m.Nodes[0]
	if nd.Name != multipassCronTargetNodeName {
		t.Fatalf("fixture sanity: expected node name %q, got %q", multipassCronTargetNodeName, nd.Name)
	}
	// exec_policy MUST be set — the reporter agent dispatches exec
	// tool-calls to this node, and a nil policy would make the
	// node-side exec handler reject the dispatch (ExecPolicyStep.
	// Applicable=false skip path). Unlike TestNodeSmoke which
	// deliberately omits it, here it's the whole point.
	if nd.ExecPolicy == nil {
		t.Fatalf("fixture sanity: node %q must declare exec_policy (agent dispatches exec to it); got nil",
			nd.Name)
	}
	if !strings.EqualFold(nd.ExecPolicy.Security, "full") || !strings.EqualFold(nd.ExecPolicy.Ask, "off") {
		t.Fatalf("fixture sanity: node %q exec_policy must be {security:full, ask:off} for cron (most permissive); got %+v",
			nd.Name, nd.ExecPolicy)
	}

	ag := &m.Agents[0]
	if ag.ID != multipassCronAgentID {
		t.Fatalf("fixture sanity: expected agent id %q, got %q", multipassCronAgentID, ag.ID)
	}
	if ag.Tools == nil || ag.Tools.Exec == nil {
		t.Fatalf("fixture sanity: agent %q must declare tools.exec (host=node + node=scraper-node); got tools=%+v",
			ag.ID, ag.Tools)
	}
	if !strings.EqualFold(ag.Tools.Exec.Host, "node") {
		t.Fatalf("fixture sanity: agent %q tools.exec.host = %q, want \"node\" (to dispatch across the ws)",
			ag.ID, ag.Tools.Exec.Host)
	}
	if ag.Tools.Exec.Node != multipassCronTargetNodeName {
		t.Fatalf("fixture sanity: agent %q tools.exec.node = %q, want %q",
			ag.ID, ag.Tools.Exec.Node, multipassCronTargetNodeName)
	}
	if !strings.HasPrefix(strings.ToLower(ag.Model.Primary), "ollama/") {
		t.Fatalf("fixture sanity: agent %q model.primary = %q, want prefix \"ollama/\" (cron test pins LLM backend to Ollama)",
			ag.ID, ag.Model.Primary)
	}

	for _, mc := range m.Machines {
		if mc.Type != manifestdata.MachineTypeMultipass {
			t.Fatalf("fixture sanity: machine %q type = %q, want %q",
				mc.Name, mc.Type, manifestdata.MachineTypeMultipass)
		}
		if strings.TrimSpace(mc.AgentUser) == "" {
			t.Fatalf("fixture sanity: machine %q has empty agent_user", mc.Name)
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

	prov, err := multipass.NewProvider(multipass.Options{})
	if err != nil {
		t.Fatalf("new multipass provider: %v", err)
	}
	dial := sshDialFunc(signer)

	t.Cleanup(func() {
		if os.Getenv("CLAWS_IT_KEEP_VMS") != "" {
			t.Logf("CLAWS_IT_KEEP_VMS set → leaving VMs up for debug (prefix=%s)", prefix)
			return
		}
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	for _, want := range []string{"provisioning", "security", "mesh-gateway", "mesh-join", "gateway", "node", "agents"} {
		if !containsStr(plan.PhaseNames(), want) {
			t.Fatalf("plan is missing %q phase; got %v", want, plan.PhaseNames())
		}
	}

	ex, err := plan.Build()
	if err != nil {
		t.Fatalf("plan.Build: %v", err)
	}

	t.Log("milestone: applying full plan (mesh + gateway + node + agents) on 2 VMs")
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

	// --- resolve bridge IPs post-apply -------------------------------------

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
	gwBridgeIP := gwInst.PublicIPv4
	t.Logf("gateway VM → %s (bridge=%s)", gwInst.Label, gwBridgeIP)

	nodeMT := findMachineTarget(t, plan, nodeMachine.Name)
	if nodeMT.Instance == nil {
		t.Fatalf("node machine %q has no Instance after apply", nodeMachine.Name)
	}
	nodeInst := nodeMT.Instance
	if nodeInst.Status != "running" {
		t.Errorf("node machine %q status = %q, want %q", nodeMachine.Name, nodeInst.Status, "running")
	}
	nodeBridgeIP := nodeInst.PublicIPv4
	t.Logf("node VM → %s (bridge=%s)", nodeInst.Label, nodeBridgeIP)

	// --- gateway + node health sanity check --------------------------------
	//
	// Before the cron-specific work, confirm the gateway daemon is
	// up and the node paired. If either is broken, the cron
	// assertions downstream will fail with a confusing symptom; a
	// direct check here pins the regression to the upstream phase
	// it belongs to. These reuse helpers that already have their
	// own dedicated tests (TestGatewaySmoke, TestNodeSmoke).

	assertGatewayUnitActive(t, dial, gwBridgeIP, gwMachine)
	assertGatewayPortListeningOnLAN(t, dial, gwBridgeIP, gwMachine)
	assertNodeUnitActive(t, dial, nodeBridgeIP, nodeMachine)
	assertNodePairedOnGateway(t, dial, gwBridgeIP, gwMachine, nd.Name)

	// --- Fake Ollama stub on the gateway VM --------------------------------
	//
	// SFTP-upload test/infra/fake-ollama.py onto the gateway and
	// start it under systemd --user. See startFakeOllamaOnMultipassVM
	// below for the boot + readiness sequence, and the package doc.go
	// for why we use a stub here instead of installing real ollama.

	t.Log("milestone: uploading + starting fake-ollama on gateway VM")
	startFakeOllamaOnMultipassVM(t, dial, gwBridgeIP, gwMachine)

	t.Log("milestone: sanity-checking fake-ollama on loopback")
	verifyMultipassFakeOllamaReachable(t, dial, gwBridgeIP, gwMachine)

	// --- Configure openclaw to point at the fake-ollama stub --------------
	//
	// The ollama plugin synthesizes a local auth token when the
	// provider config is "meaningful" — i.e. baseUrl differs from
	// OLLAMA_DEFAULT_BASE_URL ("http://127.0.0.1:11434"), or
	// models[] is populated, or an explicit apiKey/auth is set.
	// configureMultipassOllamaProvider uses "http://localhost:<fake
	// port>" specifically to trip the baseUrl branch while still
	// routing to the colocated fake-ollama on 127.0.0.1. Without that,
	// isolated cron-agent sessions fail with "No API key found for
	// provider 'ollama'". The plugin reloads openclaw.json per
	// discovery run, so no gateway restart is needed.

	t.Log("milestone: configuring models.providers.ollama.baseUrl")
	configureMultipassOllamaProvider(t, dial, gwBridgeIP, gwMachine)

	// --- Cron job lifecycle -------------------------------------------------

	t.Log("milestone: reading gateway auth token")
	gatewayToken := readGatewayTokenOverSSH(t, dial, gwBridgeIP, gwMachine)
	t.Logf("gateway token present (len=%d)", len(gatewayToken))

	t.Log("milestone: creating cron job (every 5s, reporter agent)")
	jobID := createMultipassCronJob(t, dial, gwBridgeIP, gwMachine, gatewayToken)
	t.Logf("cron job created: id=%q", jobID)
	t.Cleanup(func() {
		removeMultipassCronJob(t, dial, gwBridgeIP, gwMachine, gatewayToken, jobID)
	})

	t.Logf("milestone: waiting for %d cron runs (timeout %s)",
		multipassCronWantRuns, multipassCronRunsTimeout)
	runs := waitForMultipassCronRuns(t, dial, gwBridgeIP, gwMachine,
		gatewayToken, jobID, multipassCronWantRuns, multipassCronRunsTimeout)
	t.Logf("milestone: got %d cron runs", len(runs))

	// --- Assert every run succeeded ----------------------------------------

	var failed int
	for i, r := range runs {
		if r.Status == "ok" {
			t.Logf("run %d: ok (duration=%dms) summary=%q",
				i+1, r.DurationMs, multipassTruncate(r.Summary, 80))
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

// startFakeOllamaOnMultipassVM uploads test/infra/fake-ollama.py to
// the gateway VM and starts it as a systemd --user service. We use
// systemd --user (not nohup + disown) because:
//
//   - The security phase runs `loginctl enable-linger agent` so
//     user units survive SSH disconnects unconditionally; this is
//     the same mechanism the gateway's own openclaw-gateway unit
//     relies on. If fake-ollama doesn't survive, neither would the
//     gateway — which would have surfaced before cron.
//   - systemd-run cleanly captures logs (`journalctl --user -u
//     fake-ollama`) which the test can pull on failure.
//   - The unit auto-stops on VM delete, so there's no background
//     process cleanup to worry about.
//
// The script is uploaded via SFTP (sshfile.WriteFile) rather than a
// heredoc over SSH so non-trivial Python indentation + triple-quoted
// strings survive the round trip intact.
func startFakeOllamaOnMultipassVM(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()

	scriptPath := findFakeOllamaScriptMultipass(t)
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

	if err := sshfile.WriteFile(client, multipassFakeOllamaRemot, data); err != nil {
		t.Fatalf("[%s] upload fake-ollama.py to %s: %v", mc.Name, multipassFakeOllamaRemot, err)
	}
	t.Logf("[%s] fake-ollama.py uploaded (%d bytes) to %s", mc.Name, len(data), multipassFakeOllamaRemot)

	startScript := fmt.Sprintf(`set -eux
chmod +x %[2]s
systemctl --user stop %[1]s 2>/dev/null || true
systemctl --user reset-failed %[1]s 2>/dev/null || true
systemd-run --user \
    --unit=%[1]s \
    --property=Type=simple \
    --setenv=FAKE_OLLAMA_PORT=%[3]d \
    /usr/bin/python3 %[2]s
`, multipassFakeOllamaUnit, multipassFakeOllamaRemot, multipassFakeOllamaPort)

	out, err := sshRunAsGatewayAgent(t, dial, host, mc, startScript)
	if err != nil {
		t.Fatalf("[%s] systemd-run fake-ollama: %v\n%s", mc.Name, err, out)
	}

	// Wait for the stub to answer /api/tags on loopback. Python's
	// http.server needs a few hundred ms to bind; 30s is ample
	// headroom for slow disk caches on fresh VMs.
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := sshRunAsGatewayAgent(t, dial, host, mc,
			fmt.Sprintf(`curl -sS --max-time 3 http://127.0.0.1:%d/api/tags 2>&1 || echo "(curl failed)"`, multipassFakeOllamaPort))
		if err == nil && strings.Contains(out, multipassOllamaModel) {
			t.Logf("[%s] fake-ollama ready on :%d: %s", mc.Name, multipassFakeOllamaPort, multipassTruncate(out, 120))
			return
		}
		last = out
		time.Sleep(1 * time.Second)
	}
	journal, _ := sshRunAsGatewayAgent(t, dial, host, mc,
		fmt.Sprintf(`journalctl --user -u %s --no-pager -n 40 2>&1 || true`, multipassFakeOllamaUnit))
	t.Fatalf("[%s] fake-ollama never answered on :%d (last curl: %s)\n--- journal ---\n%s",
		mc.Name, multipassFakeOllamaPort, last, journal)
}

// verifyMultipassFakeOllamaReachable pings /api/chat on the fake to
// prove the stub is alive + returns the expected tool-call payload
// before handing off to the cron scheduler. A failure here pins the
// regression to fake-ollama itself rather than to the openclaw
// ollama plugin or the cron scheduler.
func verifyMultipassFakeOllamaReachable(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
	t.Helper()
	payload := fmt.Sprintf(
		`curl -sS --max-time 5 http://127.0.0.1:%d/api/chat `+
			`-H "Content-Type: application/json" `+
			`-d '{"messages":[{"role":"user","content":"probe"}]}'`,
		multipassFakeOllamaPort,
	)
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, payload)
	if err != nil {
		t.Fatalf("[%s] fake-ollama /api/chat probe: %v\n%s", mc.Name, err, out)
	}
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"exec"`) {
		t.Fatalf("[%s] fake-ollama didn't return an exec tool call:\n%s", mc.Name, out)
	}
	t.Logf("[%s] fake-ollama ok: %s", mc.Name, multipassTruncate(out, 120))
}

// configureMultipassOllamaProvider points the gateway's Ollama
// provider at the fake-ollama stub on 127.0.0.1:<fake port>. We use
// the hostname "localhost" (not "127.0.0.1") so the ollama plugin's
// hasMeaningfulExplicitOllamaConfig() check treats the baseUrl as
// non-default and mints a synthetic local auth token. The plugin's
// check is a literal string compare against OLLAMA_DEFAULT_BASE_URL
// ("http://127.0.0.1:11434") so any string-different-but-routing-
// equivalent URL works — "localhost" still resolves to 127.0.0.1 at
// dial time. Without this, isolated cron-agent sessions fail with
// "No API key found for provider 'ollama'" because auth-profiles.json
// for agent "reporter" doesn't inherit the synthetic key. The non-
// 11434 port also means this config can never be mistaken for "point
// at a real ollama install" — a drop of the fake would surface as
// connection-refused rather than silently hitting a stale daemon.
//
// Uses node + fs.writeFileSync (same technique as the docker cron
// test) rather than `openclaw config set --batch-json` to keep the
// payload small and obviously-readable, and to avoid depending on
// config-set's handling of nested object creation when the
// models.providers.ollama key is absent.
func configureMultipassOllamaProvider(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine) {
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
`, multipassFakeOllamaPort)
	out, err := sshRunAsGatewayAgent(t, dial, host, mc, script)
	if err != nil {
		t.Fatalf("[%s] configure ollama provider: %v\n%s", mc.Name, err, out)
	}
}

// findFakeOllamaScriptMultipass walks up from the test's working
// directory until it finds the repo root (go.mod) and returns the
// absolute path to test/infra/fake-ollama.py. Avoids hard-coding
// absolute paths so the test works from any checkout location.
func findFakeOllamaScriptMultipass(t *testing.T) string {
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
// Gateway token + cron RPC helpers (via openclaw CLI over SSH)
// ---------------------------------------------------------------------------

// readGatewayTokenOverSSH opens a dedicated SSH client and calls
// gwService.ReadToken. The helpers in multipass_gateway_test.go
// operate via per-call dial (sshRunAsGatewayAgent), but ReadToken
// needs a *xssh.Client directly so it can use sshfile/SFTP — so
// we dial once here and close after.
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

// createMultipassCronJob adds an every-5s cron job on the gateway
// and returns the id. --no-deliver disables announce delivery so
// the run doesn't fail on "Channel is required" when no messaging
// channel is configured. The cron still executes the agent turn
// end-to-end; it just won't post the summary anywhere. See
// openclaw/src/cli/cron-cli/register.cron-add.ts (opts.deliver ===
// false → delivery.mode="none").
func createMultipassCronJob(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken string) string {
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
		gatewayToken, multipassCronJobName, multipassCronAgentID, prompt, multipassCronInterval,
	)

	out, err := sshRunAsGatewayAgent(t, dial, host, mc, script)
	if err != nil {
		t.Fatalf("[%s] cron add: %v\n%s", mc.Name, err, out)
	}
	raw := extractFirstJSON(out)
	if raw == "" {
		t.Fatalf("[%s] cron add: no JSON in output:\n%s", mc.Name, out)
	}
	var job multipassCronJobJSON
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("[%s] parse cron add output: %v\nraw=%s", mc.Name, err, raw)
	}
	if job.ID == "" {
		t.Fatalf("[%s] cron add: empty job id in %s", mc.Name, raw)
	}
	return job.ID
}

// removeMultipassCronJob is best-effort — used as cleanup so a
// failed test doesn't leave a scheduler hammering Ollama between
// runs (and between tests, if the caller re-uses the VMs via
// CLAWS_IT_KEEP_VMS).
func removeMultipassCronJob(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string) {
	t.Helper()
	script := fmt.Sprintf(`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron rm %q 2>/dev/null || true`, gatewayToken, jobID)
	_, _ = sshRunAsGatewayAgent(t, dial, host, mc, script)
	t.Logf("cleanup: cron job %q removed", jobID)
}

// waitForMultipassCronRuns polls `openclaw cron runs` until at
// least wantRuns finished entries appear. The poll interval (3s)
// is tight so the log shows progress without spamming stderr.
func waitForMultipassCronRuns(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string, wantRuns int, timeout time.Duration) []multipassCronRunEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("[%s] timed out waiting for %d cron runs (job %q)", mc.Name, wantRuns, jobID)
		}
		entries := fetchMultipassCronRuns(t, dial, host, mc, gatewayToken, jobID)
		finished := filterMultipassFinished(entries)
		t.Logf("[%s] cron runs: %d finished of %d wanted (raw entries: %d)",
			mc.Name, len(finished), wantRuns, len(entries))
		if len(finished) >= wantRuns {
			return finished[:wantRuns]
		}
		time.Sleep(3 * time.Second)
	}
}

func fetchMultipassCronRuns(t *testing.T, dial provisioning.SSHDialFunc, host string, mc manifestdata.Machine, gatewayToken, jobID string) []multipassCronRunEntry {
	t.Helper()
	// `cron runs` always emits JSON via printCronJson (openclaw/
	// src/cli/cron-cli/register.cron-simple.ts). It does not
	// accept a --json flag.
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
	var page multipassCronRunsPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Logf("[%s] parse cron runs: %v\nraw=%s", mc.Name, err, raw)
		return nil
	}
	return page.Entries
}

func filterMultipassFinished(entries []multipassCronRunEntry) []multipassCronRunEntry {
	out := make([]multipassCronRunEntry, 0, len(entries))
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
// sshRunAsGatewayAgent (which caps at 45s). Ollama install / pull /
// first-generate each legitimately take multiple minutes; a 45s
// cap would false-positive those as SSH hangs. Kept as a dedicated
// helper so the short-lived path (config-get, is-active, ss -tln)
// stays bounded and the long-lived path is explicit at call sites.
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

// extractFirstJSON returns the first balanced {...} JSON object
// found in raw. `openclaw cron add` / `cron runs` may emit log
// lines before the JSON body; callers can't rely on the JSON being
// the only thing on stdout.
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

func multipassTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
