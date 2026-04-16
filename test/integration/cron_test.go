//go:build integration

package integration

// TestCronAgentWithNodeExec verifies that agent crons fire on schedule and
// successfully generate responses through a real LLM (Ollama).
//
// Topology
//   - gateway container (oc-test): runs openclaw-gateway
//   - scraper container (oc-test): runs openclaw-node (scraper-node)
//   - ollama container: qwen2.5:0.5b, reachable at http://ollama:11434 on
//     the shared Docker network
//
// Flow
//   1. Apply the full plan from manifest-cron.yml.
//   2. Configure models.providers.ollama.baseUrl so the gateway can reach the
//      ollama container, then restart openclaw-gateway.
//   3. Wait for scraper-node to re-establish its websocket.
//   4. Add a cron job on the gateway (every 5s) that prompts the "reporter"
//      agent to produce a random one-sentence reply.
//   5. Poll `openclaw cron runs` (always JSON) until cronWantRuns finished
//      entries appear, then remove the job and assert every run had
//      status=ok.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply"
	gwService "github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/gateway"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

const (
	cronJobName        = "reporter-itest"
	cronAgentID        = "reporter"
	cronTargetNodeName = "scraper-node"
	cronInterval       = "5s"
	// We only need to see the cron *fire repeatedly* end-to-end to prove the
	// pipeline works (scheduler → isolated agent → ollama model → finished
	// run). Two successful runs is enough signal without making the test
	// costly in CI — each isolated cron run cold-boots a fresh ts-node
	// agent subprocess and calls qwen2.5:0.5b on the pinned ollama image,
	// which averages ~2 min per run. Isolated sessions are serialized by
	// the queue, so back-pressure means the 5s interval yields roughly one
	// run every couple of minutes regardless.
	cronWantRuns    = 2
	cronRunsTimeout = 5 * time.Minute
)

// cronJobJSON is the subset of the CronJob object returned by `openclaw cron add`.
type cronJobJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cronRunEntry mirrors a single entry from `openclaw cron runs`.
type cronRunEntry struct {
	Action     string `json:"action"` // "finished"
	Status     string `json:"status"` // "ok" | "error" | "skipped"
	RunAtMs    int64  `json:"runAtMs"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// cronRunsPage mirrors the RPC response for `openclaw cron runs`.
type cronRunsPage struct {
	Entries []cronRunEntry `json:"entries"`
	Total   int            `json:"total"`
}

func TestCronAgentWithNodeExec(t *testing.T) {
	t.Log("milestone: TestCronAgentWithNodeExec — cron + ollama + node-exec agent")

	m, signer, gwPort, _ := setupCronTestInfra(t)
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

	t.Log("milestone: claws apply (full plan)")
	if err := ep.Execute(ctx, scaffold.ExecuteOptions{}); err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	client, err := dial(ctx, "127.0.0.1", gwPort, "root")
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer client.Close()

	// ── 1. Wait for scraper-node WebSocket to come up after apply ────────────
	// The apply phase pairs and starts the node; we must wait for the ws to
	// be live before creating the cron so the isolated session can use it.
	t.Log("milestone: wait for scraper-node websocket")
	waitNodeConnected(t, client, cronTargetNodeName, 90*time.Second)

	// ── 2. Point the gateway's Ollama provider at the test ollama container ─
	// The ollama plugin reloads openclaw.json per discovery run and
	// synthesizes a local auth token whenever baseUrl is explicit, so no
	// gateway restart (which would reset the node ws) is needed.
	t.Log("milestone: configure models.providers.ollama.baseUrl")
	configureOllamaProvider(t, client)

	// ── 4. Read the gateway auth token (needed for cron RPCs) ────────────────
	home, err := gwService.ResolveHome(client)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	gatewayToken, err := gwService.ReadToken(client, home)
	if err != nil || gatewayToken == "" {
		t.Fatalf("read gateway token: err=%v tokenLen=%d", err, len(gatewayToken))
	}
	t.Logf("gateway token present (len=%d)", len(gatewayToken))

	// ── 5. Sanity-check Ollama reachability from inside the gateway ──────────
	verifyOllamaReachable(t, client)

	// ── 6. Add a cron job: every 5s, prompt the reporter for a sentence ──────
	t.Log("milestone: create cron job (every 5s, reporter agent)")
	jobID := createCronJob(t, client, gatewayToken)
	t.Logf("cron job created: id=%q", jobID)
	t.Cleanup(func() {
		removeCronJob(t, client, gatewayToken, jobID)
	})

	// ── 7. Poll cron runs until cronWantRuns finished entries appear ─────────
	t.Logf("milestone: waiting for %d cron runs (timeout %s)", cronWantRuns, cronRunsTimeout)
	runs := waitForCronRuns(t, client, gatewayToken, jobID, cronWantRuns, cronRunsTimeout)
	t.Logf("milestone: got %d cron runs", len(runs))

	// ── 8. Assert every run succeeded ────────────────────────────────────────
	var failed int
	for i, r := range runs {
		if r.Status == "ok" {
			t.Logf("run %d: ok (duration=%dms) summary=%q", i+1, r.DurationMs, truncate(r.Summary, 80))
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
// infra
// ---------------------------------------------------------------------------

// setupCronTestInfra mirrors setupTestInfra but loads manifest-cron.yml so
// the cron test can use its own (smaller) agent/node topology.
func setupCronTestInfra(t *testing.T) (*data.Manifest, xssh.Signer, int, int) {
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

	m := loadTestManifestFile(t, "manifest-cron.yml")
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

// ---------------------------------------------------------------------------
// Ollama + gateway helpers
// ---------------------------------------------------------------------------

// configureOllamaProvider points the gateway's Ollama provider at the test
// ollama container. The ollama plugin auto-discovers every model on the box
// via `/api/tags` and synthesizes a local auth key at runtime whenever
// baseUrl differs from the default (see openclaw/extensions/ollama/index.ts
// resolveSyntheticAuth + discovery.run), so setting baseUrl alone is enough.
func configureOllamaProvider(t *testing.T, client *xssh.Client) {
	t.Helper()
	script := `set -e
node -e '
const fs = require("fs"), os = require("os");
const p = os.homedir() + "/.openclaw/openclaw.json";
const c = JSON.parse(fs.readFileSync(p, "utf8"));
if (!c.models) c.models = {};
if (!c.models.providers) c.models.providers = {};
c.models.providers.ollama = { baseUrl: "http://ollama:11434", models: [] };
fs.writeFileSync(p, JSON.stringify(c, null, 2) + "\n");
'
`
	out, err := bash.RunOutput(client, script)
	if err != nil {
		t.Fatalf("configure ollama provider: %v\n%s", err, out)
	}
}

// verifyOllamaReachable sanity-checks that the gateway container can reach
// the ollama container and that the model responds. We don't assert on the
// response text — Ollama's small models are non-deterministic.
func verifyOllamaReachable(t *testing.T, client *xssh.Client) {
	t.Helper()
	payload := `curl -sS --max-time 60 http://ollama:11434/api/generate ` +
		`-H "Content-Type: application/json" ` +
		`-d '{"model":"qwen2.5:0.5b","prompt":"Reply with one short English sentence.","stream":false}'`
	out, err := bash.RunOutput(client, payload)
	if err != nil {
		t.Fatalf("ollama generate: %v\n%s", err, out)
	}
	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("parse ollama json: %v\nraw=%s", err, out)
	}
	if strings.TrimSpace(resp.Response) == "" {
		t.Fatalf("empty ollama response: %s", out)
	}
	t.Logf("ollama ok: %q", truncate(strings.TrimSpace(resp.Response), 80))
}

// ---------------------------------------------------------------------------
// cron RPC helpers
// ---------------------------------------------------------------------------

// createCronJob adds an every-5s cron job on the gateway and returns the id.
func createCronJob(t *testing.T, client *xssh.Client, gatewayToken string) string {
	t.Helper()
	prompt := "Tell me a random one-sentence fact. " +
		"Reply with exactly one short English sentence, nothing else."
	// --no-deliver disables announce delivery so the run doesn't fail on
	// "Channel is required" when no messaging channel (telegram/slack/…) is
	// configured. The cron still executes the agent turn end-to-end; it just
	// won't post the summary anywhere. See openclaw/src/cli/cron-cli/
	// register.cron-add.ts (opts.deliver === false → delivery.mode="none").
	script := fmt.Sprintf(
		`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron add `+
			`--name %q `+
			`--agent %q `+
			`--message %q `+
			`--every %q `+
			`--session isolated `+
			`--no-deliver`,
		gatewayToken, cronJobName, cronAgentID, prompt, cronInterval,
	)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		t.Fatalf("cron add: %v\n%s", err, out)
	}
	raw := extractFirstJSON(out)
	if raw == "" {
		t.Fatalf("cron add: no JSON in output:\n%s", out)
	}
	var job cronJobJSON
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("parse cron add output: %v\nraw=%s", err, raw)
	}
	if job.ID == "" {
		t.Fatalf("cron add: empty job id in %s", raw)
	}
	return job.ID
}

// removeCronJob best-effort removes a cron job. Used as cleanup so test
// failures don't leave a scheduler hammering ollama.
func removeCronJob(t *testing.T, client *xssh.Client, gatewayToken, jobID string) {
	t.Helper()
	script := fmt.Sprintf(`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron rm %q 2>/dev/null || true`, gatewayToken, jobID)
	_, _ = bash.RunOutput(client, script)
	t.Logf("cleanup: cron job %q removed", jobID)
}

// waitForCronRuns polls `openclaw cron runs` until at least wantRuns finished
// entries appear, or the timeout fires.
func waitForCronRuns(t *testing.T, client *xssh.Client, gatewayToken, jobID string, wantRuns int, timeout time.Duration) []cronRunEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d cron runs (job %q)", wantRuns, jobID)
		}
		entries := fetchCronRuns(t, client, gatewayToken, jobID)
		finished := filterFinished(entries)
		t.Logf("cron runs: %d finished of %d wanted (raw entries: %d)",
			len(finished), wantRuns, len(entries))
		if len(finished) >= wantRuns {
			return finished[:wantRuns]
		}
		time.Sleep(3 * time.Second)
	}
}

// fetchCronRuns reads recent run entries for a cron job.
func fetchCronRuns(t *testing.T, client *xssh.Client, gatewayToken, jobID string) []cronRunEntry {
	t.Helper()
	// `cron runs` always emits JSON via printCronJson (openclaw/src/cli/cron-cli/
	// register.cron-simple.ts). It does not accept a --json flag.
	script := fmt.Sprintf(`export OPENCLAW_GATEWAY_TOKEN=%q
openclaw cron runs --id %q --limit 20`, gatewayToken, jobID)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		t.Logf("cron runs (non-fatal): %v\n%s", err, out)
		return nil
	}
	raw := extractFirstJSON(out)
	if raw == "" {
		return nil
	}
	var page cronRunsPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Logf("parse cron runs: %v\nraw=%s", err, raw)
		return nil
	}
	return page.Entries
}

// filterFinished keeps only entries that represent a completed run.
func filterFinished(entries []cronRunEntry) []cronRunEntry {
	out := make([]cronRunEntry, 0, len(entries))
	for _, e := range entries {
		if e.Action == "finished" && e.Status != "" {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// extractFirstJSON returns the first balanced {...} JSON object found in raw.
// `openclaw cron add` / `cron runs` may emit log lines before the JSON body.
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
