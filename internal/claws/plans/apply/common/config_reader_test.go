package common

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
	xssh "golang.org/x/crypto/ssh"
)

// ----- unit: extractModelPrimary / normalizeAgentID -----

func TestExtractModelPrimary(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare string", `"sonnet-4"`, "sonnet-4"},
		{"object form with primary", `{"primary":"sonnet-4","fallbacks":["opus"]}`, "sonnet-4"},
		{"object form with primary trimmed", `{"primary":"  opus-3  "}`, "opus-3"},
		{"null", `null`, ""},
		{"empty", ``, ""},
		{"object without primary", `{"fallbacks":["x"]}`, ""},
		{"number (invalid) falls through", `42`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractModelPrimary(json.RawMessage(c.raw))
			if got != c.want {
				t.Fatalf("extractModelPrimary(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestExtractModelFallbacks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"bare string", `"sonnet-4"`, nil},
		{"object form with fallbacks", `{"primary":"sonnet-4","fallbacks":["opus","haiku"]}`, []string{"opus", "haiku"}},
		{"object form without fallbacks", `{"primary":"opus-3"}`, nil},
		{"object form empty fallbacks", `{"primary":"opus-3","fallbacks":[]}`, []string{}},
		{"null", `null`, nil},
		{"empty", ``, nil},
		{"number (invalid) falls through", `42`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractModelFallbacks(json.RawMessage(c.raw))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("extractModelFallbacks(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestNormalizeAgentID(t *testing.T) {
	cases := map[string]string{
		"":              "",
		" main ":        "main",
		"Main":          "main",
		"CODE_reviewer": "code_reviewer",
	}
	for in, want := range cases {
		if got := normalizeAgentID(in); got != want {
			t.Fatalf("normalizeAgentID(%q) = %q, want %q", in, got, want)
		}
	}
}

// ----- unit: snapshot-backed reads against a hand-authored config -----

// sampleSnapshot seeds a snapshot with the shapes the apply steps care
// about: mixed model forms, a route binding, channel accounts, and
// tools.elevated toggles.
func sampleSnapshot(t *testing.T) *openclawSnapshot {
	t.Helper()
	raw := `{
	  "agents": {
	    "defaults": {"model": "default-model"},
	    "list": [
	      {"id": "Main", "model": "sonnet-4", "tools": {"exec": {"host":"h","node":"n","security":"sandbox"}}},
	      {"id": "second", "model": {"primary":"opus-3","fallbacks":["sonnet-4"]}},
	      {"id": "third"}
	    ]
	  },
	  "bindings": [
	    {"type":"route","agentId":"Main","match":{"channel":"telegram","accountId":"main-bot"}},
	    {"type":"route","agentId":"other","match":{"channel":"slack","accountId":"ops"}},
	    {"type":"mention","agentId":"Main","match":{"channel":"telegram","accountId":"x"}}
	  ],
	  "channels": {
	    "telegram": {
	      "defaultAccount": "main-bot",
	      "accounts": {"main-bot": {}, "secondary-bot": {}}
	    },
	    "slack": {"accounts": {"ops": {}}}
	  },
	  "tools": {"elevated": {"enabled": true, "allowFrom": {"telegram": ["admin"]}}},
	  "gateway": {"mode": "local", "bind": "lan"}
	}`
	var snap openclawSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	return &snap
}

// cachingProbeCtx returns a ctx that carries a plan cache with
// probe-active set and the given snapshot preinstalled, so reader calls
// never have to fetch over SFTP. Also returns the cache key we used.
func cachingProbeCtx(t *testing.T, host ConfigHost, snap *openclawSnapshot) context.Context {
	t.Helper()
	ctx := scaffold.EnsurePlanCache(context.Background())
	scaffold.SetProbeActive(ctx, true)
	scaffold.PlanCacheSet(ctx, snapshotCacheKey(host), snap)
	return ctx
}

// countingReader counts delegations. Snapshot reader falls back to it
// outside the probe phase; inside the probe phase with a primed cache it
// must never delegate.
type countingReader struct {
	ConfigReader
	calls int
}

func (c *countingReader) AgentConfigIndex(ctx context.Context, _ *xssh.Client, h ConfigHost, id string) (int, error) {
	c.calls++
	return -1, nil
}
func (c *countingReader) AgentModel(ctx context.Context, _ *xssh.Client, h ConfigHost, id string) (string, bool, error) {
	c.calls++
	return "", false, nil
}
func (c *countingReader) AgentModelFull(ctx context.Context, _ *xssh.Client, h ConfigHost, id string) (string, []string, bool, error) {
	c.calls++
	return "", nil, false, nil
}
func (c *countingReader) AgentTools(ctx context.Context, _ *xssh.Client, h ConfigHost, idx int) (*RemoteToolsConfig, error) {
	c.calls++
	return &RemoteToolsConfig{}, nil
}
func (c *countingReader) Elevated(ctx context.Context, _ *xssh.Client, h ConfigHost) (*RemoteElevatedConfig, error) {
	c.calls++
	return &RemoteElevatedConfig{}, nil
}
func (c *countingReader) AgentBindings(ctx context.Context, _ *xssh.Client, h ConfigHost, id string) ([]RemoteBinding, error) {
	c.calls++
	return nil, nil
}
func (c *countingReader) ChannelAccounts(ctx context.Context, _ *xssh.Client, h ConfigHost) (map[string][]string, error) {
	c.calls++
	return nil, nil
}
func (c *countingReader) DefaultAccount(ctx context.Context, _ *xssh.Client, h ConfigHost, kind string) (string, error) {
	c.calls++
	return "", nil
}
func (c *countingReader) GatewayView(ctx context.Context, _ *xssh.Client, h ConfigHost) (GatewayView, error) {
	c.calls++
	return GatewayView{}, nil
}
func (c *countingReader) DeviceList(ctx context.Context, _ *xssh.Client, h ConfigHost) (*DeviceList, error) {
	c.calls++
	return &DeviceList{}, nil
}

// Each test below primes a cached snapshot so there's no real SFTP read.

func TestSnapshotReader_AgentConfigIndex_normalizesID(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	fb := &countingReader{}
	r := NewSnapshotConfigReader(fb).(*snapshotConfigReader)

	idx, err := r.AgentConfigIndex(ctx, nil, host, "MAIN")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (case-insensitive match of Main)", idx)
	}
	idx, _ = r.AgentConfigIndex(ctx, nil, host, "absent")
	if idx != -1 {
		t.Fatalf("idx = %d, want -1 for absent agent", idx)
	}
	if fb.calls != 0 {
		t.Fatalf("fallback called %d times; should be 0 inside probe with primed cache", fb.calls)
	}
}

func TestSnapshotReader_AgentModel_stringAndObjectForms(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	// string form
	m, exists, err := r.AgentModel(ctx, nil, host, "main")
	if err != nil || !exists || m != "sonnet-4" {
		t.Fatalf("main: got (%q, %v, %v), want (sonnet-4, true, nil)", m, exists, err)
	}
	// object form
	m, exists, _ = r.AgentModel(ctx, nil, host, "second")
	if !exists || m != "opus-3" {
		t.Fatalf("second: got (%q, %v), want (opus-3, true)", m, exists)
	}
	// unset → falls back to agents.defaults.model
	m, exists, _ = r.AgentModel(ctx, nil, host, "third")
	if !exists || m != "default-model" {
		t.Fatalf("third: got (%q, %v), want (default-model, true)", m, exists)
	}
	// absent
	_, exists, _ = r.AgentModel(ctx, nil, host, "nope")
	if exists {
		t.Fatalf("absent agent: exists = true, want false")
	}
}

func TestSnapshotReader_AgentModelFull_stringAndObjectForms(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	// string form - no fallbacks
	m, fb, exists, err := r.AgentModelFull(ctx, nil, host, "main")
	if err != nil || !exists || m != "sonnet-4" {
		t.Fatalf("main: got (%q, %v, %v, %v), want (sonnet-4, nil, true, nil)", m, fb, exists, err)
	}
	if fb != nil {
		t.Fatalf("main fallbacks: got %v, want nil", fb)
	}

	// object form with fallbacks
	m, fb, exists, _ = r.AgentModelFull(ctx, nil, host, "second")
	if !exists || m != "opus-3" {
		t.Fatalf("second: got (%q, %v), want (opus-3, true)", m, exists)
	}
	wantFb := []string{"sonnet-4"}
	if !reflect.DeepEqual(fb, wantFb) {
		t.Fatalf("second fallbacks: got %v, want %v", fb, wantFb)
	}

	// unset model → falls back to agents.defaults.model, no fallbacks
	m, fb, exists, _ = r.AgentModelFull(ctx, nil, host, "third")
	if !exists || m != "default-model" {
		t.Fatalf("third: got (%q, %v), want (default-model, true)", m, exists)
	}
	if fb != nil {
		t.Fatalf("third fallbacks: got %v, want nil (defaults don't inherit fallbacks)", fb)
	}

	// absent
	_, _, exists, _ = r.AgentModelFull(ctx, nil, host, "nope")
	if exists {
		t.Fatalf("absent agent: exists = true, want false")
	}
}

func TestSnapshotReader_AgentTools_byIndex(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	tc, err := r.AgentTools(ctx, nil, host, 0)
	if err != nil || tc == nil || tc.Exec == nil || tc.Exec.Host != "h" {
		t.Fatalf("agent 0 tools: %+v err=%v", tc, err)
	}
	// missing tools → empty config
	tc, err = r.AgentTools(ctx, nil, host, 2)
	if err != nil || tc == nil || tc.Exec != nil {
		t.Fatalf("agent 2 tools: %+v err=%v, want zero-value", tc, err)
	}
	// out of range → zero-value
	tc, err = r.AgentTools(ctx, nil, host, 99)
	if err != nil || tc == nil || tc.Exec != nil {
		t.Fatalf("oob tools: %+v err=%v", tc, err)
	}
}

func TestSnapshotReader_AgentBindings_filtersRouteAndAgent(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	bs, err := r.AgentBindings(ctx, nil, host, "MAIN")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("bindings = %+v; want exactly the one type=route for main", bs)
	}
	got := bs[0]
	want := RemoteBinding{AgentID: "Main", Channel: "telegram", Account: "main-bot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("binding = %+v, want %+v", got, want)
	}
}

func TestSnapshotReader_ChannelAccounts(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	accts, err := r.ChannelAccounts(ctx, nil, host)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tg := accts["telegram"]
	sort.Strings(tg)
	if !reflect.DeepEqual(tg, []string{"main-bot", "secondary-bot"}) {
		t.Fatalf("telegram accounts = %v, want [main-bot secondary-bot]", tg)
	}
	if !reflect.DeepEqual(accts["slack"], []string{"ops"}) {
		t.Fatalf("slack accounts = %v, want [ops]", accts["slack"])
	}
}

func TestSnapshotReader_DefaultAccount(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	got, err := r.DefaultAccount(ctx, nil, host, "telegram")
	if err != nil || got != "main-bot" {
		t.Fatalf("telegram default = %q err=%v, want main-bot", got, err)
	}
	got, _ = r.DefaultAccount(ctx, nil, host, "slack")
	if got != "" {
		t.Fatalf("slack default = %q, want empty (unset)", got)
	}
	got, _ = r.DefaultAccount(ctx, nil, host, "discord")
	if got != "" {
		t.Fatalf("discord default = %q, want empty (absent kind)", got)
	}
}

func TestSnapshotReader_Elevated(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	r := NewSnapshotConfigReader(&countingReader{}).(*snapshotConfigReader)

	elev, err := r.Elevated(ctx, nil, host)
	if err != nil || elev == nil || elev.Enabled == nil || !*elev.Enabled {
		t.Fatalf("elevated = %+v err=%v, want enabled=true", elev, err)
	}
	if !reflect.DeepEqual(elev.AllowFrom["telegram"], []string{"admin"}) {
		t.Fatalf("elevated.AllowFrom = %+v", elev.AllowFrom)
	}
}

func TestSnapshotReader_GatewayView_fromSnapshot(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	snap := sampleSnapshot(t)
	ctx := cachingProbeCtx(t, host, snap)
	fb := &countingReader{}
	r := NewSnapshotConfigReader(fb).(*snapshotConfigReader)

	v, err := r.GatewayView(ctx, nil, host)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v.Mode != "local" || v.Bind != "lan" {
		t.Fatalf("gateway view = %+v, want {local lan}", v)
	}
	if fb.calls != 0 {
		t.Fatalf("fallback called %d times; snapshot should have served gateway view", fb.calls)
	}
}

func TestSnapshotReader_GatewayView_emptySnapshot(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	// Empty snapshot == missing openclaw.json on a fresh machine.
	// GatewayView must return zero-values, not fall back to CLI.
	ctx := scaffold.EnsurePlanCache(context.Background())
	scaffold.SetProbeActive(ctx, true)
	scaffold.PlanCacheSet(ctx, snapshotCacheKey(host), &openclawSnapshot{})

	fb := &countingReader{}
	r := NewSnapshotConfigReader(fb).(*snapshotConfigReader)

	v, err := r.GatewayView(ctx, nil, host)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v.Mode != "" || v.Bind != "" {
		t.Fatalf("fresh-machine view = %+v, want zero-values", v)
	}
	if fb.calls != 0 {
		t.Fatalf("fallback called %d times; empty snapshot is still a valid answer", fb.calls)
	}
}

func TestSnapshotReader_DeviceList_fromSnapshot(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	// Prime the devices-snapshot cache directly — this test exercises
	// the read path, not the SFTP fetch path (which needs a live client).
	ctx := scaffold.EnsurePlanCache(context.Background())
	scaffold.SetProbeActive(ctx, true)
	primed := &DeviceList{
		Pending: []PendingDevice{{RequestID: "req-1", DisplayName: "node-a", Role: "node"}},
		Paired:  []PairedDevice{{DisplayName: "cli-1", ClientMode: "cli"}},
	}
	scaffold.PlanCacheSet(ctx, devicesSnapshotCacheKey(host), primed)

	fb := &countingReader{}
	r := NewSnapshotConfigReader(fb).(*snapshotConfigReader)

	dl, err := r.DeviceList(ctx, nil, host)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !reflect.DeepEqual(dl, primed) {
		t.Fatalf("device list = %+v, want %+v", dl, primed)
	}
	// Second call must hit the cache, not the fallback.
	if _, err := r.DeviceList(ctx, nil, host); err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if fb.calls != 0 {
		t.Fatalf("fallback called %d times; devices snapshot should have served both", fb.calls)
	}
}

// ----- probe-flag gating -----

// When IsProbeActive is false the snapshot reader MUST delegate to the
// fallback on every call so Execute sees live state. Verify each method.
func TestSnapshotReader_ExecutePhase_delegatesToFallback(t *testing.T) {
	host := ConfigHost{Addr: "h", Port: 22, User: "u"}
	ctx := scaffold.EnsurePlanCache(context.Background())
	scaffold.SetProbeActive(ctx, false)
	// Prime both caches. The reader must NOT use them because probe is
	// off; it should call the fallback on every method instead.
	scaffold.PlanCacheSet(ctx, snapshotCacheKey(host), sampleSnapshot(t))
	scaffold.PlanCacheSet(ctx, devicesSnapshotCacheKey(host), &DeviceList{})

	fb := &countingReader{}
	r := NewSnapshotConfigReader(fb).(*snapshotConfigReader)

	_, _ = r.AgentConfigIndex(ctx, nil, host, "main")
	_, _, _ = r.AgentModel(ctx, nil, host, "main")
	_, _ = r.AgentTools(ctx, nil, host, 0)
	_, _ = r.Elevated(ctx, nil, host)
	_, _ = r.AgentBindings(ctx, nil, host, "main")
	_, _ = r.ChannelAccounts(ctx, nil, host)
	_, _ = r.DefaultAccount(ctx, nil, host, "telegram")
	_, _ = r.GatewayView(ctx, nil, host)
	_, _ = r.DeviceList(ctx, nil, host)

	if fb.calls != 9 {
		t.Fatalf("fallback call count = %d, want 9 (one per method)", fb.calls)
	}
}
