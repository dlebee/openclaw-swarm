package multipass

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

// fakeRunner is a table-driven Runner that matches the first arg ("launch",
// "info", "list", "delete") and returns canned stdout / error. Each entry
// represents one expected call and is consumed in order, so tests can
// encode a full launch→info-polling→delete sequence as a slice.
type fakeRunner struct {
	calls []fakeCall
	// seen captures every invocation — tests can assert on argument shape
	// (e.g. "launch received --cpus 2") without hand-wiring more plumbing.
	seen []fakeInvocation
	idx  int
}

type fakeCall struct {
	// matchCmd is matched against args[0]. Empty matches any. Tests that
	// need stricter matching (by e.g. label) can inspect seen after the fact.
	matchCmd string
	stdout   string
	err      error
}

type fakeInvocation struct {
	args  []string
	stdin string
}

func (f *fakeRunner) Run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	var body string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	f.seen = append(f.seen, fakeInvocation{args: append([]string{}, args...), stdin: body})

	if f.idx >= len(f.calls) {
		return "", errors.New("fakeRunner: unexpected call: " + strings.Join(args, " "))
	}
	c := f.calls[f.idx]
	f.idx++
	if c.matchCmd != "" && (len(args) == 0 || args[0] != c.matchCmd) {
		return "", errors.New("fakeRunner: expected " + c.matchCmd + ", got " + strings.Join(args, " "))
	}
	return c.stdout, c.err
}

const runningInfoJSON = `{
  "info": {
    "vm-a": {"state": "Running", "ipv4": ["192.168.64.10"]}
  }
}`

const startingInfoJSON = `{
  "info": {
    "vm-a": {"state": "Starting", "ipv4": []}
  }
}`

func newTestProvider(t *testing.T, r *fakeRunner) *Provider {
	t.Helper()
	p, err := NewProvider(Options{
		Runner: r,
		TagDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	p.waitPollInterval = time.Millisecond // keep tests fast
	return p
}

func TestCreateInstance_launchAndPollsForIP(t *testing.T) {
	r := &fakeRunner{
		calls: []fakeCall{
			{matchCmd: "launch"},                            // launch returns empty stdout
			{matchCmd: "info", stdout: startingInfoJSON},    // first poll: no IP yet
			{matchCmd: "info", stdout: runningInfoJSON},     // second poll: IP
		},
	}
	p := newTestProvider(t, r)
	inst, err := p.CreateInstance(context.Background(), hosting.CreateInstanceOpts{
		Label:      "vm-a",
		Image:      "24.04",
		Tags:       []string{"claws/demo", "claws/demo/web"},
		PublicKeys: []string{"ssh-ed25519 AAA test@local"},
		CPUs:       2,
		Memory:     "1G",
		Disk:       "5G",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst == nil {
		t.Fatal("nil instance")
	}
	if inst.PublicIPv4 != "192.168.64.10" {
		t.Errorf("PublicIPv4 = %q, want 192.168.64.10", inst.PublicIPv4)
	}
	if inst.Provider != hosting.KindMultipass {
		t.Errorf("Provider = %q, want %q", inst.Provider, hosting.KindMultipass)
	}
	if inst.ResourceID != "vm-a" || inst.Label != "vm-a" {
		t.Errorf("Resource/Label = %q/%q, want vm-a/vm-a", inst.ResourceID, inst.Label)
	}

	launch := r.seen[0]
	if launch.args[0] != "launch" {
		t.Fatalf("first call was %v", launch.args)
	}
	joined := strings.Join(launch.args, " ")
	for _, want := range []string{"--name vm-a", "--cpus 2", "--memory 1G", "--disk 5G", "24.04"} {
		if !strings.Contains(joined, want) {
			t.Errorf("launch args missing %q: %s", want, joined)
		}
	}
	if !strings.Contains(launch.stdin, "#cloud-config") {
		t.Errorf("cloud-init seed missing header: %q", launch.stdin)
	}
	if !strings.Contains(launch.stdin, "ssh-ed25519 AAA test@local") {
		t.Errorf("cloud-init seed missing key")
	}
}

func TestCreateInstance_omitsSizingFlagsWhenDefaultsRequested(t *testing.T) {
	r := &fakeRunner{
		calls: []fakeCall{
			{matchCmd: "launch"},
			{matchCmd: "info", stdout: runningInfoJSON},
		},
	}
	p := newTestProvider(t, r)
	_, err := p.CreateInstance(context.Background(), hosting.CreateInstanceOpts{Label: "vm-a"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.seen[0].args, " ")
	for _, forbidden := range []string{"--cpus", "--memory", "--disk"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("launch should not contain %q when fields are zero, got: %s", forbidden, joined)
		}
	}
}

func TestListByTag_matchesViaSidecar(t *testing.T) {
	const listOut = `{"list":[{"name":"vm-a"},{"name":"vm-b"},{"name":"vm-c"}]}`
	r := &fakeRunner{
		calls: []fakeCall{
			{matchCmd: "list", stdout: listOut},
			// only vm-a is tagged for "claws/demo"; vm-b/vm-c have no
			// sidecar and should be skipped before info is called.
			{matchCmd: "info", stdout: runningInfoJSON},
		},
	}
	p := newTestProvider(t, r)

	if err := p.tags.save("vm-a", []string{"claws/demo", "claws/demo/web"}); err != nil {
		t.Fatal(err)
	}
	if err := p.tags.save("vm-b", []string{"other/tag"}); err != nil {
		t.Fatal(err)
	}

	insts, err := p.ListByTag(context.Background(), "claws/demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 {
		t.Fatalf("expected 1 instance, got %d: %+v", len(insts), insts)
	}
	if insts[0].Label != "vm-a" {
		t.Errorf("matched wrong label: %q", insts[0].Label)
	}
	if insts[0].PublicIPv4 != "192.168.64.10" {
		t.Errorf("IP = %q", insts[0].PublicIPv4)
	}
}

func TestDeleteInstance_idempotentOnMissing(t *testing.T) {
	r := &fakeRunner{
		calls: []fakeCall{
			{matchCmd: "delete", err: errors.New("multipass delete: instance \"vm-a\" does not exist")},
		},
	}
	p := newTestProvider(t, r)
	if err := p.DeleteInstance(context.Background(), "vm-a"); err != nil {
		t.Fatalf("DeleteInstance should swallow 'does not exist', got: %v", err)
	}
}

func TestDeleteInstance_removesSidecar(t *testing.T) {
	r := &fakeRunner{calls: []fakeCall{{matchCmd: "delete"}}}
	p := newTestProvider(t, r)
	if err := p.tags.save("vm-a", []string{"claws/demo"}); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteInstance(context.Background(), "vm-a"); err != nil {
		t.Fatal(err)
	}
	tags, err := p.tags.load("vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Errorf("expected sidecar cleared, got %v", tags)
	}
}

func TestInfo_notFoundIsTyped(t *testing.T) {
	r := &fakeRunner{
		calls: []fakeCall{
			{matchCmd: "info", err: errors.New("multipass info: instance \"vm-x\" does not exist")},
		},
	}
	p := newTestProvider(t, r)
	_, err := p.info(context.Background(), "vm-x")
	if !errors.Is(err, errInstanceNotFound) {
		t.Fatalf("want errInstanceNotFound, got %v", err)
	}
}

func TestProvider_Kind(t *testing.T) {
	r := &fakeRunner{}
	p := newTestProvider(t, r)
	if got := p.Kind(); got != hosting.KindMultipass {
		t.Fatalf("Kind = %q, want %q", got, hosting.KindMultipass)
	}
}

func TestBuildCloudInit_ubuntuPath_includesKeysAndHeader(t *testing.T) {
	// Default / ubuntu bootstrap, no hostname: keys land only on the
	// cloud-image default user; root-specific hardening escape hatches
	// stay off; mDNS/hostname machinery stays off.
	out := buildCloudInit(
		[]string{"ssh-ed25519 AAA one", " ", "ssh-ed25519 BBB two"},
		"ubuntu",
		"",
	)
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Errorf("missing #cloud-config header: %q", out)
	}
	for _, want := range []string{"ssh-ed25519 AAA one", "ssh-ed25519 BBB two", "users:", "default"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, disallow := range []string{"disable_root", "write_files", "/root/.ssh", "hostname:", "avahi-daemon", "runcmd:"} {
		if strings.Contains(out, disallow) {
			t.Errorf("ubuntu path must not emit %q:\n%s", disallow, out)
		}
	}
}

func TestBuildCloudInit_rootPath_enablesRootSSHAndSeedsRootKeys(t *testing.T) {
	// Root bootstrap, no hostname: (1) disable_root: false so cloud-init
	// doesn't lock root out, (2) a write_files block installing the keys
	// into /root/.ssh/authorized_keys, (3) a runcmd chmod to bring the
	// parent dir down to 0700 so sshd's StrictModes accepts it. Must
	// still NOT install avahi — mDNS machinery is tied to the hostname
	// input, not the bootstrap user.
	out := buildCloudInit(
		[]string{"ssh-ed25519 ROOTKEY label"},
		"root",
		"",
	)
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Errorf("missing #cloud-config header: %q", out)
	}
	for _, want := range []string{
		"disable_root: false",
		"write_files:",
		"/root/.ssh/authorized_keys",
		"permissions: '0600'",
		"runcmd:",
		"/root/.ssh",
		"ssh-ed25519 ROOTKEY label",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, disallow := range []string{"hostname:", "avahi-daemon", "5353/udp"} {
		if strings.Contains(out, disallow) {
			t.Errorf("no-hostname root path must not emit %q:\n%s", disallow, out)
		}
	}
}

func TestBuildCloudInit_emptyBootstrapUser_keepsUbuntuPath(t *testing.T) {
	// Empty string is the wire default for Linode callers (who don't care
	// about this opt). It must behave like "ubuntu": no root-specific
	// escape hatches.
	out := buildCloudInit([]string{"ssh-ed25519 AAA one"}, "", "")
	if strings.Contains(out, "disable_root") || strings.Contains(out, "write_files") {
		t.Errorf("empty bootstrapUser must not trigger root path:\n%s", out)
	}
}

func TestBuildCloudInit_hostnamePath_setsHostnameAndInstallsAvahi(t *testing.T) {
	// With a hostname, cloud-init must (a) pin the VM's short hostname +
	// FQDN so uname -n is predictable, (b) install avahi-daemon +
	// libnss-mdns so peers can resolve `<hostname>.local` both ways,
	// (c) stage a ufw rule for UDP 5353 so the later security-phase
	// `ufw --force enable` doesn't black-hole multicast.
	out := buildCloudInit(
		[]string{"ssh-ed25519 AAA one"},
		"ubuntu",
		"gateway-host",
	)
	for _, want := range []string{
		"hostname: gateway-host",
		"fqdn: gateway-host.local",
		"manage_etc_hosts: true",
		"packages:",
		"- avahi-daemon",
		"- libnss-mdns",
		"runcmd:",
		"[ systemctl, enable, --now, avahi-daemon ]",
		"[ ufw, allow, 5353/udp, comment, 'mdns' ]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildCloudInit_hostnameAndRoot_singleRuncmdBlock(t *testing.T) {
	// When both the hostname path and the root path emit runcmd entries,
	// they must share a single `runcmd:` block — two sibling keys with
	// the same name is invalid YAML and cloud-init would reject the
	// whole document, silently falling back to defaults (which on the
	// root path means a locked-out root account).
	out := buildCloudInit(
		[]string{"ssh-ed25519 ROOTKEY label"},
		"root",
		"gateway-host",
	)
	if got := strings.Count(out, "\nruncmd:\n"); got != 1 {
		t.Fatalf("expected exactly one `runcmd:` block, got %d in:\n%s", got, out)
	}
	// Both feature branches' commands must appear under that single
	// block — spot-check one entry from each.
	for _, want := range []string{
		"[ systemctl, enable, --now, avahi-daemon ]",
		"[ chmod, '0700', /root/.ssh ]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in merged runcmd:\n%s", want, out)
		}
	}
}
