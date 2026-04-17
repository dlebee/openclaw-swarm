package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	"github.com/gluwa/openclaw-swarm2/internal/platformutil/bash"
	xssh "golang.org/x/crypto/ssh"
)

// ChannelAccounts is the JSON structure returned by `openclaw channels list --json`.
// Keys are channel kinds (telegram, slack, discord); values are account name slices.
type ChannelAccounts map[string][]string

// ListChannelAccounts runs `openclaw channels list --json` and returns
// a map of kind -> []accountName.
func ListChannelAccounts(client *xssh.Client) (ChannelAccounts, error) {
	out, err := bash.RunOutput(client, `openclaw channels list --json 2>/dev/null || echo "{}"`)
	if err != nil {
		return ChannelAccounts{}, nil
	}
	raw := extractJSON(strings.TrimSpace(out), '{')

	var payload struct {
		Chat ChannelAccounts `json:"chat"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ChannelAccounts{}, nil
	}
	if payload.Chat == nil {
		return ChannelAccounts{}, nil
	}
	return payload.Chat, nil
}

// AccountExists reports whether accountName is already registered for the given kind.
func AccountExists(accounts ChannelAccounts, kind, name string) bool {
	for _, n := range accounts[kind] {
		if n == name {
			return true
		}
	}
	return false
}

// ReadDefaultAccount reads `channels.<kind>.defaultAccount` from the remote config.
func ReadDefaultAccount(client *xssh.Client, kind string) (string, error) {
	key := fmt.Sprintf("channels.%s.defaultAccount", kind)
	out, err := bash.RunOutput(client, fmt.Sprintf(
		`openclaw config get %s 2>/dev/null || echo ""`, key))
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

const (
	conflictRetries = 3
	conflictDelay   = 2 * time.Second
)

// RunWithConflictRetry executes a script and retries on ConfigMutationConflictError.
func RunWithConflictRetry(client *xssh.Client, script string) error {
	var lastErr error
	for attempt := 0; attempt <= conflictRetries; attempt++ {
		out, err := bash.RunOutput(client, script)
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%w\n%s", err, out)
		if !strings.Contains(out, "ConfigMutationConflictError") {
			return lastErr
		}
		if attempt < conflictRetries {
			time.Sleep(time.Duration(attempt+1) * conflictDelay)
		}
	}
	return fmt.Errorf("config conflict after %d retries: %w", conflictRetries, lastErr)
}

// RunWithConflictAndTransientRetry executes a script on host:port as user and
// applies two layers of retry:
//
//  1. Transient SSH session retry via common.RunBashOutputWithRetry — a fresh
//     dial is performed on "exited without exit status" / EOF / reset errors,
//     which have been observed mid-command on the gateway during channel
//     mutations (the remote sshd and/or Tailscale routing state can change
//     beneath a long-lived pooled connection).
//  2. ConfigMutationConflictError retry — openclaw serialises config writes
//     and returns a 409-style error to losers when two processes race. Each
//     conflict attempt re-dials via (1).
//
// Use this for any channel-phase command that mutates gateway config
// (`openclaw channels add`, `openclaw config set`). For fast reads, the
// pooled client is fine.
func RunWithConflictAndTransientRetry(
	ctx context.Context,
	dial common.SSHDialFunc,
	host string,
	port int,
	user, script string,
) error {
	var lastErr error
	for attempt := 0; attempt <= conflictRetries; attempt++ {
		out, err := common.RunBashOutputWithRetry(ctx, dial, host, port, user, script)
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%w\n%s", err, out)
		if !strings.Contains(out, "ConfigMutationConflictError") {
			return lastErr
		}
		if attempt < conflictRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * conflictDelay):
			}
		}
	}
	return fmt.Errorf("config conflict after %d retries: %w", conflictRetries, lastErr)
}

// PairChannel approves a channel pairing request on the gateway via
// `openclaw pairing approve <kind> <code>`.
func PairChannel(client *xssh.Client, kind, code string) error {
	script := fmt.Sprintf(`openclaw pairing approve %s %q`, kind, code)
	out, err := bash.RunOutput(client, script)
	if err != nil {
		return fmt.Errorf("pair channel %s: %w\n%s", kind, err, out)
	}
	return nil
}

// RemoveChannelAccount removes a channel account via
// `openclaw channels remove --channel <kind> --account <name> --delete`.
func RemoveChannelAccount(client *xssh.Client, kind, name string) error {
	script := fmt.Sprintf(
		`openclaw channels remove --channel %s --account %q --delete`,
		kind, name)
	if err := RunWithConflictRetry(client, script); err != nil {
		return fmt.Errorf("remove channel %s/%s: %w", kind, name, err)
	}
	return nil
}

// extractJSON finds the first occurrence of startChar in the output and
// returns from there onward. Handles non-JSON preamble from the CLI.
func extractJSON(s string, startChar byte) string {
	idx := strings.IndexByte(s, startChar)
	if idx < 0 {
		if startChar == '[' {
			return "[]"
		}
		return "{}"
	}
	return s[idx:]
}
