package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigGet runs `openclaw config get <path> --json` on the remote host and
// returns the raw JSON value. The caller unmarshals into the appropriate type.
// Sensitive paths (e.g. gateway.auth.token) are redacted by the CLI.
func (s *Service) ConfigGet(ctx context.Context, h Host, path string) (json.RawMessage, error) {
	out, err := s.shellOutput(ctx, h,
		fmt.Sprintf(`openclaw config get %q --json`, path))
	if err != nil {
		return nil, fmt.Errorf("config get %s: %w", path, err)
	}
	raw := json.RawMessage(strings.TrimSpace(out))
	if !json.Valid(raw) {
		return nil, fmt.Errorf("config get %s: invalid json", path)
	}
	return raw, nil
}

// Version runs `openclaw --version` on the remote host.
func (s *Service) Version(ctx context.Context, h Host) (string, error) {
	out, err := s.shellOutput(ctx, h, `openclaw --version`)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	return strings.TrimSpace(out), nil
}
