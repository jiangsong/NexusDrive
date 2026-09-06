// Package service installs and removes the per-user mount supervisor
// (systemd on Linux, launchd on macOS). It was cmd/cloudfs-only code; it
// moved here so the running daemon can report and manage its own service over
// the control plane without the CLI in the loop, and so a single place owns
// the one invariant that matters: the supervisor must never be left able to
// relaunch the process after the mount has been detached.
package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"cloudfs/internal/config"
)

// Label is the launchd service label and the base of the systemd unit name.
const Label = "io.cloudfs.mount"

// Runtime is everything service management touches, with its side-effecting
// parts injected so a test drives it without a real service manager. The
// zero value is not usable; build one with Real.
type Runtime struct {
	GOOS       string
	Home       string
	ConfigDir  string
	Executable string
	UID        int
	Out        io.Writer
	// Run executes a service-manager command (systemctl, launchctl). Injected
	// so tests observe the exact invocations and their order.
	Run func(string, ...string) ([]byte, error)
	// Mounted reports whether a path is a live mount point, and Unmount
	// detaches one. Uninstall uses them to guarantee the mount is gone before
	// the file that would relaunch it is removed.
	Mounted func(string) (bool, error)
	Unmount func(string) error
}

// Real builds the Runtime that talks to the machine's own service manager.
func Real() (Runtime, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Runtime{}, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Runtime{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return Runtime{}, err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return Runtime{
		GOOS: runtime.GOOS, Home: home, ConfigDir: configDir, Executable: executable,
		UID: currentUID(), Out: os.Stdout,
		Run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
		Mounted: MountpointMounted,
		Unmount: Unmount,
	}, nil
}

// Supported reports whether this platform has a service manager this package
// drives, and why not when it does not.
func (rt Runtime) Supported() (bool, string) {
	switch rt.GOOS {
	case "linux", "darwin":
		return true, ""
	default:
		return false, fmt.Sprintf("%s has no per-user service integration; use a native service manager", rt.GOOS)
	}
}

// File is the absolute path of the service definition for this platform.
func (rt Runtime) File() (string, error) {
	switch rt.GOOS {
	case "linux":
		return filepath.Join(rt.ConfigDir, "systemd", "user", "cloudfs.service"), nil
	case "darwin":
		return filepath.Join(rt.Home, "Library", "LaunchAgents", Label+".plist"), nil
	default:
		return "", fmt.Errorf("service: %s is unsupported; use a native service manager", rt.GOOS)
	}
}

// Installed reports whether the service definition exists on disk. The control
// plane uses it to answer a second install with 409 rather than silently
// re-enabling a service the user may have deliberately stopped.
func (rt Runtime) Installed() (bool, error) {
	file, err := rt.File()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(file)
	if statErr == nil {
		return true, nil
	}
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	return false, statErr
}

// Status writes the service manager's own status report to rt.Out.
func (rt Runtime) Status() error {
	file, err := rt.File()
	if err != nil {
		return err
	}
	if rt.Run == nil {
		return errors.New("service: command runner is unavailable")
	}
	out := rt.Out
	if out == nil {
		out = io.Discard
	}
	var output []byte
	switch rt.GOOS {
	case "linux":
		output, err = rt.Run("systemctl", "--user", "status", "--no-pager", "cloudfs.service")
	case "darwin":
		output, err = rt.Run("launchctl", "print", fmt.Sprintf("gui/%d/%s", rt.UID, Label))
	}
	if len(output) > 0 {
		_, _ = out.Write(output)
	}
	if err != nil {
		return fmt.Errorf("service: status (%s): %w", file, err)
	}
	return nil
}

// Install writes the service definition and asks the manager to enable and
// start it. A config with at least one mount is required, because the unit
// runs `cloudfs mount` against it.
func (rt Runtime) Install(cfg *config.Config, configPath string) error {
	file, mountPath, configPath, err := rt.prepare(cfg, configPath)
	if err != nil {
		return err
	}
	// Serialize against a concurrent install/uninstall (a second UI tab, the
	// CLI and the control plane at once): the loser waits rather than racing
	// two managers over one unit file.
	unlock, err := rt.lock()
	if err != nil {
		return err
	}
	defer unlock()

	var body []byte
	switch rt.GOOS {
	case "linux":
		body = []byte(renderSystemdUnit(rt.Executable, configPath, mountPath))
	case "darwin":
		body = []byte(renderLaunchdPlist(rt.Executable, configPath, mountPath))
	}
	if err := writeServiceFile(file, body); err != nil {
		return err
	}
	switch rt.GOOS {
	case "linux":
		if out, err := rt.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return commandError("reload systemd", out, err)
		}
		if out, err := rt.Run("systemctl", "--user", "enable", "--now", "cloudfs.service"); err != nil {
			return commandError("enable systemd service", out, err)
		}
	case "darwin":
		domain := fmt.Sprintf("gui/%d", rt.UID)
		_, _ = rt.Run("launchctl", "bootout", domain+"/"+Label)
		if out, err := rt.Run("launchctl", "bootstrap", domain, file); err != nil {
			return commandError("bootstrap launchd service", out, err)
		}
	}
	fmt.Fprintf(rt.writer(), "installed and started %s\n", file)
	return nil
}

// Uninstall stops the service, detaches any mount it left behind, and removes
// the definition — strictly in that order. Removing the file first, or leaving
// the manager able to relaunch, could have the supervisor bring the process
// back up onto a mount point that was just pulled out from under it.
func (rt Runtime) Uninstall(cfg *config.Config, configPath string) error {
	file, mountPath, _, err := rt.prepare(cfg, configPath)
	if err != nil {
		return err
	}
	unlock, err := rt.lock()
	if err != nil {
		return err
	}
	defer unlock()

	_, statErr := os.Stat(file)
	installed := statErr == nil
	switch rt.GOOS {
	case "linux":
		if out, err := rt.Run("systemctl", "--user", "disable", "--now", "cloudfs.service"); err != nil && installed {
			return commandError("disable systemd service", out, err)
		}
	case "darwin":
		domain := fmt.Sprintf("gui/%d", rt.UID)
		if _, activeErr := rt.Run("launchctl", "print", domain+"/"+Label); activeErr == nil {
			if out, err := rt.Run("launchctl", "bootout", domain+"/"+Label); err != nil {
				return commandError("bootout launchd service", out, err)
			}
		}
	}
	if rt.Mounted != nil {
		mounted, err := rt.Mounted(mountPath)
		if err != nil {
			return fmt.Errorf("service: inspect mount point: %w", err)
		}
		if mounted {
			if rt.Unmount == nil {
				return errors.New("service: mount remains but no unmount helper is available")
			}
			if err := rt.Unmount(mountPath); err != nil {
				return fmt.Errorf("service: detach remaining mount before uninstall: %w", err)
			}
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", file, err)
	}
	if rt.GOOS == "linux" {
		if out, err := rt.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return commandError("reload systemd", out, err)
		}
	}
	fmt.Fprintf(rt.writer(), "uninstalled %s\n", file)
	return nil
}

// prepare resolves and validates the file, mount and config paths common to
// install and uninstall.
func (rt Runtime) prepare(cfg *config.Config, configPath string) (file, mountPath, absConfig string, err error) {
	file, err = rt.File()
	if err != nil {
		return "", "", "", err
	}
	if rt.Run == nil {
		return "", "", "", errors.New("service: command runner is unavailable")
	}
	if cfg == nil || len(cfg.Mounts) == 0 {
		return "", "", "", errors.New("service: a config with at least one mount is required")
	}
	absConfig, err = filepath.Abs(configPath)
	if err != nil {
		return "", "", "", err
	}
	mountPath, err = filepath.Abs(cfg.Mounts[0].Path)
	if err != nil {
		return "", "", "", err
	}
	for _, value := range []string{rt.Executable, absConfig, mountPath} {
		if err := safeServiceValue(value); err != nil {
			return "", "", "", err
		}
	}
	return file, mountPath, absConfig, nil
}

func (rt Runtime) writer() io.Writer {
	if rt.Out == nil {
		return io.Discard
	}
	return rt.Out
}

func safeServiceValue(value string) error {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("service: executable, config and mount paths must be nonempty single-line values")
	}
	return nil
}

func systemdQuote(value string) string {
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "$$", "%", "%%").Replace(value)
	return "\"" + value + "\""
}

func renderSystemdUnit(executable, configPath, mountPath string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=CloudFS user mount",
		"Wants=network-online.target",
		"After=network-online.target",
		"RequiresMountsFor=" + systemdQuote(mountPath),
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + systemdQuote(executable) + " mount " + systemdQuote(mountPath) + " --config " + systemdQuote(configPath) + " --foreground",
		"Restart=on-failure",
		"RestartSec=5s",
		"TimeoutStopSec=30s",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

func xmlText(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func renderLaunchdPlist(executable, configPath, mountPath string) string {
	args := []string{executable, "mount", mountPath, "--config", configPath, "--foreground"}
	var arguments strings.Builder
	for _, arg := range args {
		arguments.WriteString("\n      <string>")
		arguments.WriteString(xmlText(arg))
		arguments.WriteString("</string>")
	}
	return strings.Join([]string{
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>",
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">",
		"<plist version=\"1.0\">",
		"<dict>",
		"  <key>Label</key>",
		"  <string>" + Label + "</string>",
		"  <key>ProgramArguments</key>",
		"  <array>" + arguments.String(),
		"  </array>",
		"  <key>RunAtLoad</key>",
		"  <true/>",
		"  <key>KeepAlive</key>",
		"  <dict>",
		"    <key>SuccessfulExit</key>",
		"    <false/>",
		"  </dict>",
		"  <key>ProcessType</key>",
		"  <string>Interactive</string>",
		"</dict>",
		"</plist>",
		"",
	}, "\n")
}

func writeServiceFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("service: create directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".cloudfs-service-*")
	if err != nil {
		return fmt.Errorf("service: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temp.Write(body); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("service: install %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ok = true
	return nil
}

func commandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if len(message) > 1024 {
		message = message[:1024]
	}
	if message == "" {
		return fmt.Errorf("service: %s: %w", action, err)
	}
	return fmt.Errorf("service: %s: %w: %s", action, err, message)
}
