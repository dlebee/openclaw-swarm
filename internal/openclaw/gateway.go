package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReadGatewayToken reads the gateway auth token from the remote host via SFTP.
// The token cannot be obtained via `openclaw config get` because the
// gateway.auth.token path is marked sensitive and gets redacted.
// We first try the dedicated token file, then fall back to parsing
// the full config JSON.
func (s *Service) ReadGatewayToken(ctx context.Context, h Host) (string, error) {
	data, err := s.readFile(ctx, h, ".openclaw/.gateway-token")
	if err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read gateway token: %w", err)
	}

	data, err = s.readFile(ctx, h, ".openclaw/openclaw.json")
	if err != nil {
		return "", fmt.Errorf("read gateway config: %w", err)
	}
	var cfg struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse gateway config: %w", err)
	}
	if cfg.Gateway.Auth.Token == "" {
		return "", fmt.Errorf("gateway auth token is empty")
	}
	return cfg.Gateway.Auth.Token, nil
}
