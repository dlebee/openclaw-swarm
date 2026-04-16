package openclaw

import "context"

// ChannelsList runs `openclaw channels list --json` on the remote host.
func (s *Service) ChannelsList(ctx context.Context, h Host) (*ChannelsListResult, error) {
	return shellJSON[*ChannelsListResult](ctx, s, h, `openclaw channels list --json`)
}
