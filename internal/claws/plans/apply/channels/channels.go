// Package channels is the channels phase of the apply plan.
// Steps register bot channel accounts on gateways.
package channels

import (
	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/common"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	"github.com/gluwa/openclaw-swarm2/internal/scaffold"
)

// maxChannelConcurrency caps parallel gateway targets. Channels on different
// gateways are independent so can run in parallel.
const maxChannelConcurrency = 5

// SSHDialFunc opens an SSH client to a remote host.
type SSHDialFunc = common.SSHDialFunc

// ChannelTarget is stored on scaffold.Target.Payload for channel phase cells.
// One target per gateway that has channels defined.
type ChannelTarget struct {
	Gateway  manifestdata.Gateway
	Machine  manifestdata.Machine
	Channels []manifestdata.Channel
	// Tokens maps token_env name -> resolved secret value.
	// Populated at build time, never persisted.
	Tokens map[string]string
}

// BuildChannelTargets creates scaffold targets from gateways that have
// channels. tokenResolver is called once per unique token_env to obtain the
// secret value; it should read from os.Getenv or the manifest env_file.
func BuildChannelTargets(
	gateways []manifestdata.Gateway,
	machines []manifestdata.Machine,
	tokenResolver func(envName string) (string, error),
) ([]scaffold.Target, error) {
	machByName := make(map[string]manifestdata.Machine, len(machines))
	for _, m := range machines {
		machByName[m.Name] = m
	}

	var targets []scaffold.Target
	for _, gw := range gateways {
		if len(gw.Channels) == 0 {
			continue
		}
		tokens := make(map[string]string, len(gw.Channels))
		for _, ch := range gw.Channels {
			if ch.TokenEnv == "" {
				continue
			}
			if _, ok := tokens[ch.TokenEnv]; ok {
				continue
			}
			tok, err := tokenResolver(ch.TokenEnv)
			if err != nil {
				return nil, err
			}
			tokens[ch.TokenEnv] = tok
		}
		targets = append(targets, scaffold.Target{
			ID: gw.Name,
			Payload: &ChannelTarget{
				Gateway:  gw,
				Machine:  machByName[gw.Reference],
				Channels: gw.Channels,
				Tokens:   tokens,
			},
		})
	}
	return targets, nil
}

// Options configures the channels phase.
type Options struct {
	SSHDial SSHDialFunc
	// ConfigReader is consulted by every step's Check to probe gateway
	// state. When nil, AddPhase installs the default snapshot-over-CLI
	// reader (one SFTP read of ~/.openclaw/openclaw.json per gateway
	// cached across the entire probe pass, transparent passthrough to the
	// CLI during Execute/Verify). Tests can inject a fake here.
	ConfigReader common.ConfigReader
}

// AddPhase registers the "channels" phase.
func AddPhase(p *scaffold.Plan, targets []scaffold.Target, opts Options) *scaffold.Phase {
	if opts.ConfigReader == nil {
		opts.ConfigReader = common.DefaultConfigReader(opts.SSHDial)
	}
	ph := p.AddPhase("channels")
	n := len(targets)
	if n < 1 {
		n = 1
	}
	if n > maxChannelConcurrency {
		n = maxChannelConcurrency
	}
	ph.Concurrency = n
	ph.AddTargets(targets...)
	ph.AddStep(NewAddChannelStep(opts))
	ph.AddStep(NewEnsureDefaultStep(opts))
	return ph
}
