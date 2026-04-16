package openclaw

import "context"

// NodesList runs `openclaw nodes list --json` on the remote host.
func (s *Service) NodesList(ctx context.Context, h Host) (*PairingList, error) {
	return shellJSON[*PairingList](ctx, s, h, `openclaw nodes list --json`)
}

// NodesStatus runs `openclaw nodes status --json` on the remote host.
func (s *Service) NodesStatus(ctx context.Context, h Host) (*NodesStatusResult, error) {
	return shellJSON[*NodesStatusResult](ctx, s, h, `openclaw nodes status --json`)
}
