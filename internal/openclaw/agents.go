package openclaw

import (
	"context"
	"fmt"
)

// AgentsList runs `openclaw agents list --json` on the remote host.
func (s *Service) AgentsList(ctx context.Context, h Host) ([]AgentSummary, error) {
	return shellJSON[[]AgentSummary](ctx, s, h, `openclaw agents list --json`)
}

// AgentBindings runs `openclaw agents bindings --agent <id> --json` on the remote host.
func (s *Service) AgentBindings(ctx context.Context, h Host, agentID string) ([]AgentBinding, error) {
	return shellJSON[[]AgentBinding](ctx, s, h,
		fmt.Sprintf(`openclaw agents bindings --agent %q --json`, agentID))
}
