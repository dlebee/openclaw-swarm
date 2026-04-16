// Package systemd manages systemd services on a remote Linux host over SSH.
// Supports both system-level (sudo systemctl) and user-level (systemctl --user)
// units. Linux/systemd specific; not available on Darwin or non-systemd distros.
package systemd
