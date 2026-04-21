package manifestcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/gluwa/openclaw-swarm2/internal/manifests/data"
	manifestsvc "github.com/gluwa/openclaw-swarm2/internal/manifests/service"
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

	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// RenderOptions controls optional debug output.
type RenderOptions struct {
	Debug           bool
	ManifestAbsPath string
}

// RenderManifest formats a manifest for terminal display (Charm / Lip Gloss).
func RenderManifest(displayPath string, m *data.Manifest, termWidth int, opts RenderOptions) string {
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

	overview := overviewBox(m, tableW, opts)
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

	if opts.Debug && hasChannelTokens(m) {
		b.WriteString(sectionStyle.Render("Secrets"))
		b.WriteString("\n")
		b.WriteString(secretsTable(m, opts.ManifestAbsPath, tableW))
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

	if len(m.Automations) > 0 {
		b.WriteString(sectionStyle.Render(fmt.Sprintf("Automations (%d)", len(m.Automations))))
		b.WriteString("\n")
		b.WriteString(automationsTable(m.Automations, tableW))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func overviewBox(m *data.Manifest, w int, opts RenderOptions) string {
	lines := []string{
		mutedStyle.Render("prefix") + "  " + emptyDash(m.Prefix),
		mutedStyle.Render("node") + "    " + optionalInt(m.NodeMajor),
	}

	if opts.Debug {
		lines = append(lines, envDebugLines(m, opts.ManifestAbsPath)...)
		lines = append(lines, linodeDebugLines(m, opts.ManifestAbsPath)...)
	} else {
		lines = append(lines,
			mutedStyle.Render("env")+"     "+emptyDash(m.EnvFile),
			mutedStyle.Render("linode")+"  "+emptyDash(m.LinodeTokenEnv),
		)
	}

	body := strings.Join(lines, "\n")
	return boxStyle.Width(w).Render(body)
}

func envDebugLines(m *data.Manifest, absPath string) []string {
	if m.EnvFile == "" {
		return []string{mutedStyle.Render("env") + "     " + mutedStyle.Render("— (not set)")}
	}
	dir := filepath.Dir(absPath)
	resolved := filepath.Join(dir, filepath.FromSlash(m.EnvFile))
	exists := "yes"
	if _, err := os.Stat(resolved); err != nil {
		exists = warnStyle.Render("NO")
	}
	return []string{
		mutedStyle.Render("env") + "     " + m.EnvFile,
		mutedStyle.Render("  path") + "  " + resolved,
		mutedStyle.Render("  exists") + " " + exists,
	}
}

func linodeDebugLines(m *data.Manifest, absPath string) []string {
	if m.LinodeTokenEnv == "" {
		return []string{mutedStyle.Render("linode") + "  " + mutedStyle.Render("— (not set)")}
	}
	val, err := manifestsvc.LookupEnvFromManifest(absPath, m, m.LinodeTokenEnv)
	if err != nil {
		return []string{
			mutedStyle.Render("linode") + "  " + m.LinodeTokenEnv,
			mutedStyle.Render("  value") + " " + warnStyle.Render(err.Error()),
		}
	}
	return []string{
		mutedStyle.Render("linode") + "  " + m.LinodeTokenEnv,
		mutedStyle.Render("  value") + " " + maskSecret(val) + "  " + mutedStyle.Render(envSource(m.LinodeTokenEnv)),
	}
}

func hasChannelTokens(m *data.Manifest) bool {
	for _, gw := range m.Gateways {
		for _, ch := range gw.Channels {
			if ch.TokenEnv != "" {
				return true
			}
		}
	}
	return false
}

func secretsTable(m *data.Manifest, absPath string, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Gateway", "Channel", "Env var", "Value", "Source").
		Width(w)
	for _, gw := range m.Gateways {
		for _, ch := range gw.Channels {
			if ch.TokenEnv == "" {
				continue
			}
			val, err := manifestsvc.LookupEnvFromManifest(absPath, m, ch.TokenEnv)
			if err != nil {
				t.Row(gw.Name, ch.Name, ch.TokenEnv, warnStyle.Render("ERROR"), err.Error())
			} else {
				t.Row(gw.Name, ch.Name, ch.TokenEnv, maskSecret(val), envSource(ch.TokenEnv))
			}
		}
	}
	return t.String()
}

func maskSecret(s string) string {
	if len(s) <= 10 {
		return "****"
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

func envSource(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return "process env"
	}
	return "env_file"
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return mutedStyle.Render("—")
	}
	return s
}

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
		Headers("Name", "Type", "Region", "Host", "Bootstrap user", "Agent user").
		Width(w)
	for _, r := range rows {
		host := r.Host
		if host == "" {
			host = "—"
		}
		t.Row(r.Name, string(r.Type), r.Region, host, r.BootstrapUser, r.AgentUser)
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

func automationsTable(rows []data.Automation, w int) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		Headers("Name", "Steps", "Machines", "Manual").
		Width(w)
	for _, r := range rows {
		manual := "no"
		if r.Manual {
			manual = "yes"
		}
		t.Row(r.Name, fmt.Sprintf("%d", len(r.Steps)), strings.Join(r.Machines, ", "), manual)
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
