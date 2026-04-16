package systemd

import (
	"fmt"
	"sort"
	"strings"
)

// RestartPolicy controls when systemd restarts a service.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNo        RestartPolicy = "no"
)

// UnitSpec declaratively describes a systemd service unit.
type UnitSpec struct {
	Name        string            // unit name without .service suffix
	Description string            // [Unit] Description=
	ExecStart   string            // [Service] ExecStart=
	Env         map[string]string // [Service] Environment= lines
	Restart     RestartPolicy     // [Service] Restart=
	RestartSec  int               // [Service] RestartSec= (seconds, 0 omits)
	LogPath     string            // if set, StandardOutput/Error=append:<path>
	UserMode    bool              // systemctl --user vs sudo systemctl
}

// Render produces the .service unit file content from the spec.
func (u UnitSpec) Render() string {
	var b strings.Builder

	b.WriteString("[Unit]\n")
	desc := u.Description
	if desc == "" {
		desc = u.Name
	}
	fmt.Fprintf(&b, "Description=%s\n", desc)
	b.WriteString("After=network.target\n")

	b.WriteString("\n[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", u.ExecStart)

	if len(u.Env) > 0 {
		keys := make([]string, 0, len(u.Env))
		for k := range u.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "Environment=%s=%s\n", k, u.Env[k])
		}
	}

	if u.LogPath != "" {
		fmt.Fprintf(&b, "StandardOutput=append:%s\n", u.LogPath)
		fmt.Fprintf(&b, "StandardError=append:%s\n", u.LogPath)
	}

	restart := u.Restart
	if restart == "" {
		restart = RestartAlways
	}
	fmt.Fprintf(&b, "Restart=%s\n", restart)

	if u.RestartSec > 0 {
		fmt.Fprintf(&b, "RestartSec=%d\n", u.RestartSec)
	}

	b.WriteString("\n[Install]\n")
	if u.UserMode {
		b.WriteString("WantedBy=default.target\n")
	} else {
		b.WriteString("WantedBy=multi-user.target\n")
	}

	return b.String()
}

// ServiceFileName returns the .service filename.
func (u UnitSpec) ServiceFileName() string {
	return u.Name + ".service"
}
