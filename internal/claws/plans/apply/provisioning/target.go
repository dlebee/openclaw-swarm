package provisioning

import (
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
	manifestdata "github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

// MachineTarget is stored on scaffold.Target.Payload for create-machine cells.
type MachineTarget struct {
	Spec     manifestdata.Machine
	Instance *hosting.Instance // populated by Check (existing) or Execute (new)
}
