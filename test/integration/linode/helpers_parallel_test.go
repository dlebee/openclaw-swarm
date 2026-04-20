//go:build integration_linode

package linode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

// injectFakeTelegramTokens makes a test's channel-token plumbing
// parallel-safe.
//
// Background: `t.Setenv` mutates the process environment, which Go's
// test runtime refuses to do inside a t.Parallel() test (and would
// race anyway if two tests planted different values under the same
// name). This helper sidesteps both problems by:
//
//  1. rewriting every Gateway.Channel.TokenEnv on the loaded manifest
//     to a per-test-unique name (suffixed with the already-randomized
//     m.Prefix, uppercased and with '-' → '_') — two parallel tests
//     therefore reference DIFFERENT env var names, so they cannot
//     collide in process env or in the env_file, and the user's
//     ambient env can't accidentally satisfy the lookup either.
//
//  2. writing a tiny .env file in t.TempDir() that maps each unique
//     name to a deterministic fake token value, and pointing
//     m.EnvFile at its absolute path. LookupEnvFromManifest consults
//     os.Getenv first and falls back to env_file second; because the
//     uniquified names are never present in process env, the env_file
//     always wins.
//
// Must be called AFTER m.Prefix has been randomized. Safe to call
// even when the fixture has no channels — returns without touching
// the manifest if nothing has a TokenEnv.
//
// tokens lets the caller pin the fake value for a specific channel
// (keyed by Channel.Name); any channel not listed in the map gets a
// deterministic placeholder derived from the channel name and the
// randomized prefix. Pass nil if the test doesn't care about the
// exact token value (most tests just need "some non-empty string"
// so `openclaw channels add` is happy).
func injectFakeTelegramTokens(t *testing.T, m *manifestdata.Manifest, tokens map[string]string) {
	t.Helper()
	if m == nil {
		t.Fatalf("injectFakeTelegramTokens: manifest is nil")
	}

	suffix := envSuffixFromPrefix(m.Prefix)
	var lines []string
	for gi := range m.Gateways {
		gw := &m.Gateways[gi]
		for ci := range gw.Channels {
			ch := &gw.Channels[ci]
			original := strings.TrimSpace(ch.TokenEnv)
			if original == "" {
				continue
			}
			unique := original + "_" + suffix
			fakeToken := tokens[ch.Name]
			if strings.TrimSpace(fakeToken) == "" {
				fakeToken = fmt.Sprintf("fake-%s-%s", ch.Name, m.Prefix)
			}
			lines = append(lines, unique+"="+fakeToken)
			ch.TokenEnv = unique
		}
	}
	if len(lines) == 0 {
		return
	}

	dir := t.TempDir()
	envPath := filepath.Join(dir, "it-tokens.env")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(envPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	m.EnvFile = envPath
}

// envSuffixFromPrefix turns "it-lin-ch-a3f17b" into
// "IT_LIN_CH_A3F17B" — a legal POSIX-ish env-var name component. We
// uppercase and swap '-' for '_' so the resulting TokenEnv (e.g.
// TELEGRAM_MAIN_BOT_TOKEN_IT_LIN_CH_A3F17B) is unambiguous in the
// env_file and can never shadow a real token the developer might
// have exported in their shell.
func envSuffixFromPrefix(prefix string) string {
	s := strings.ToUpper(strings.TrimSpace(prefix))
	s = strings.ReplaceAll(s, "-", "_")
	return s
}
