// Package destroy lists and deletes Linode instances tagged for a manifest prefix.
package destroy

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gluwa/openclaw-swarm2/internal/claws/plans/apply/provisioning"
	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

// ListInstances returns all instances that carry the deployment tag claws/<prefix>.
func ListInstances(ctx context.Context, provider hosting.Provider, prefix string) ([]hosting.Instance, error) {
	if provider == nil {
		return nil, fmt.Errorf("destroy: hosting provider is required")
	}
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("destroy: manifest prefix is empty")
	}
	tag := provisioning.ClawsPrefixTag(prefix)
	return provider.ListByTag(ctx, tag)
}

// DeleteInstances removes each instance by resource ID. Stops on first API error.
func DeleteInstances(ctx context.Context, provider hosting.Provider, instances []hosting.Instance, log io.Writer) error {
	for i := range instances {
		inst := instances[i]
		if log != nil {
			fmt.Fprintf(log, "destroying %s (id=%s, region=%s)…\n", inst.Label, inst.ResourceID, inst.Region)
		}
		if err := provider.DeleteInstance(ctx, inst.ResourceID); err != nil {
			return fmt.Errorf("delete %s (%s): %w", inst.Label, inst.ResourceID, err)
		}
	}
	return nil
}

// FormatInstanceLine is a single-line summary for pickers and dry-run output.
func FormatInstanceLine(inst hosting.Instance) string {
	ip := strings.TrimSpace(inst.PublicIPv4)
	if ip == "" {
		ip = "—"
	}
	return fmt.Sprintf("%s  id=%s  %s  %s", inst.Label, inst.ResourceID, inst.Region, ip)
}
