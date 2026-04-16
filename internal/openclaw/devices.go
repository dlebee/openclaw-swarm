package openclaw

import "context"

// DevicesList runs `openclaw devices list --json` on the remote host.
func (s *Service) DevicesList(ctx context.Context, h Host) (*DevicePairingList, error) {
	return shellJSON[*DevicePairingList](ctx, s, h, `openclaw devices list --json`)
}
