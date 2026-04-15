package manifestcmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")).
			MarginBottom(1)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginBottom(1)

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("213")).
			MarginTop(1).
			MarginBottom(0)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("99")).
			Padding(0, 1).
			MarginBottom(1)

	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

// RenderManifest formats a manifest for terminal display (Charm / Lip Gloss).
func RenderManifest(displayPath string, m *data.Manifest, termWidth int) string {
	if termWidth < 20 {
		termWidth = 80
	}
	tableW := termWidth - 4
	if tableW < 40 {
		tableW = 40
	}

	var b strings.Builder

	title := titleStyle.Render("Manifest")
	sub := subtitleStyle.Render(displayPath)
	b.WriteString(lipgloss.JoinVertical(lipgloss.Left, title, sub))
	b.WriteString("\n")

	overview := overviewBox(m, tableW)
	b.WriteString(overview)
	b.WriteString("\n")

	if len(m.Machines) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Machines (%d)", len(m.Machines))))
		b.WriteString("\n")
		b.WriteString(machinesTable(m.Machines, tableW))
		b.WriteString("\n")
	}

	if len(m.Gateways) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Gateways (%d)", len(m.Gateways))))
		b.WriteString("\n")
		b.WriteString(gatewaysTable(m.Gateways, tableW))
		b.WriteString("\n")
	}

	if len(m.Nodes) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Nodes (%d)", len(m.Nodes))))
		b.WriteString("\n")
		b.WriteString(nodesTable(m.Nodes, tableW))
		b.WriteString("\n")
	}

	if len(m.Agents) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Agents (%d)", len(m.Agents))))
		b.WriteString("\n")
		b.WriteString(agentsTable(m.Agents, tableW))
		b.WriteString("\n")
	}

	if len(m.KubernetesClusters) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Kubernetes clusters (%d)", len(m.KubernetesClusters))))
		b.WriteString("\n")
		b.WriteString(kubernetesTable(m.KubernetesClusters, tableW))
		b.WriteString("\n")
	}

	if len(m.Automations) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Automations (%d)", len(m.Automations))))
		b.WriteString("\n")
		b.WriteString(automationsTable(m.Automations, tableW))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func overviewBox(m *data.Manifest, w int) string {
	lines := []string{
		mutedStyle.Render("prefix") + "  " + emptyDash(m.Prefix),
		mutedStyle.Render("node") + "    " + optionalInt(m.NodeMajor),
		mutedStyle.Render("env") + "    " + emptyDash(m.EnvFile),
		mutedStyle.Render("linode") + " " + emptyDash(m.LinodeTokenEnv),
	}
	body := strings.Join(lines, "\n")
	return boxStyle.Width(w).Render(body)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return mutedStyle.Render("—")
	}
	return s
}

// optionalInt renders unset zero as a muted placeholder (same as missing strings in the overview).
func optionalInt(n int) string {
	if n == 0 {
		return mutedStyle.Render("—")
	}
	return fmt.Sprintf("%d", n)
}

func machinesTable(rows []data.Machine, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Type", "Region", "Host", "SSH user", "Agent").
		Width(w)
	for _, r := range rows {
		host := r.Host
		if host == "" {
			host = "—"
		}
		t.Row(r.Name, string(r.Type), r.Region, host, r.SSHUser, r.AgentUser)
	}
	return t.String()
}

func gatewaysTable(rows []data.Gateway, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Machine", "OpenClaw", "Channels").
		Width(w)
	for _, r := range rows {
		ch := fmt.Sprintf("%d", len(r.Channels))
		t.Row(r.Name, r.Reference, emptyVer(r.OpenclawVersion), ch)
	}
	return t.String()
}

func emptyVer(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func nodesTable(rows []data.Node, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Gateway", "Machine").
		Width(w)
	for _, r := range rows {
		t.Row(r.Name, r.Gateway, r.Reference)
	}
	return t.String()
}

func agentsTable(rows []data.Agent, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("ID", "Gateway", "Primary model", "Workspace").
		Width(w)
	for _, r := range rows {
		t.Row(r.ID, r.Gateway, r.Model.Primary, truncate(r.Workspace, 48))
	}
	return t.String()
}

func kubernetesTable(rows []data.KubernetesCluster, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Cluster", "Kubeconfig").
		Width(w)
	for _, r := range rows {
		kc := r.Kubeconfig
		if kc == "" {
			kc = r.KubeconfigEnv
		}
		t.Row(r.Name, r.ClusterName, truncate(kc, 40))
	}
	return t.String()
}

func automationsTable(rows []data.Automation, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Kind", "Machines").
		Width(w)
	for _, r := range rows {
		t.Row(r.Name, emptyDash(r.Kind), strings.Join(r.Machines, ", "))
	}
	return t.String()
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	if max < 4 {
		return s
	}
	return s[:max-1] + "…"
}

// DisplayPath returns a short path for titles.
func DisplayPath(p string) string {
	return filepath.Clean(p)
}
